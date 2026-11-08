package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"
)

// Outcome is how a gate run ended.
type Outcome int

const (
	Pass        Outcome = iota // the check ran and passed
	Cached                     // a receipt already proves this tree
	Failed                     // the check exited non-zero
	ConfigError                // no usable config, or the command was not found
	TreeChanged                // the work tree changed while the check ran
	TimedOut                   // the deadline passed (waiting for a slot or running)
	Interrupted                // the gate was asked to stop (SIGINT/SIGTERM)
)

var outcomeNames = map[Outcome]string{
	Pass: "pass", Cached: "cached", Failed: "failed", ConfigError: "config-error",
	TreeChanged: "tree-changed", TimedOut: "timed-out", Interrupted: "interrupted",
}

func (o Outcome) String() string { return outcomeNames[o] }

// ExitCode is the `newproj gate` process exit status for o (outside hook
// mode): 0 pass, 1 check failed, 2 config error, 3 tree changed, 4 timed
// out, 130 interrupted.
func (o Outcome) ExitCode() int {
	switch o {
	case Pass, Cached:
		return 0
	case Failed:
		return 1
	case ConfigError:
		return 2
	case TreeChanged:
		return 3
	case TimedOut:
		return 4
	default:
		return 130
	}
}

// Passed reports whether o proves the tree.
func (o Outcome) Passed() bool { return o == Pass || o == Cached }

// Options configures Run.
type Options struct {
	Root     string    // work tree top level (see TopLevel)
	NoCache  bool      // run the check even if a receipt proves the tree
	Out      io.Writer // check output; nil discards it
	Log      io.Writer // the gate's own progress lines; nil discards them
	Slots    int       // concurrent checks allowed; 0 means SlotCount()
	StateDir string    // machine-local state; "" means the default
	Version  string    // gate version for receipts; "" means Version()
}

// Result describes a finished gate run.
type Result struct {
	Outcome  Outcome
	Tree     string        // tree the outcome applies to ("" if never hashed)
	Command  string        // the configured check command
	ExitCode int           // the check's exit status, when it ran
	Duration time.Duration // time spent running the check
	Changed  []string      // paths that changed during the check (TreeChanged)
	Detail   string        // one-line explanation of a non-pass outcome
	Warning  string        // non-fatal problem (e.g. receipt not recorded)
}

// Version is this binary's module version, or "(devel)" for a local build.
func Version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}

// Run gates the work tree at opts.Root: it proves the tree passes its
// configured check, using a stored receipt when one already proves it.
// A deadline or cancellation on ctx bounds the whole run, including the wait
// for a machine-wide slot. Errors that are not gate outcomes (git failing,
// unreadable state) are returned as errors.
func Run(ctx context.Context, opts Options) (Result, error) {
	logf := func(format string, args ...any) {
		if opts.Log != nil {
			_, _ = fmt.Fprintf(opts.Log, "gate: "+format+"\n", args...)
		}
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	version := opts.Version
	if version == "" {
		version = Version()
	}
	state := opts.StateDir
	if state == "" {
		var err error
		if state, err = stateDir(); err != nil {
			return Result{}, err
		}
	}

	cfg, err := LoadConfig(opts.Root)
	if err != nil {
		return Result{Outcome: ConfigError, Detail: err.Error()}, nil
	}
	res := Result{Command: cfg.Gate.Command}

	pre, err := TreeHash(ctx, opts.Root)
	if err != nil {
		return res, err
	}
	res.Tree = pre
	if !opts.NoCache {
		r, err := ReadReceipt(ctx, opts.Root, pre)
		if err != nil {
			return res, err
		}
		if r != nil && r.satisfies(cfg, version) {
			res.Outcome = Cached
			res.Detail = fmt.Sprintf("cached pass (%s, %s ago)", short(pre), time.Since(r.At).Round(time.Second))
			logf("%s", res.Detail)
			return res, nil
		}
	}

	n := opts.Slots
	if n == 0 {
		if n, err = SlotCount(); err != nil {
			return res, err
		}
	}
	waitStart := time.Now()
	slot, err := AcquireSlot(ctx, filepath.Join(state, "locks"), n, func() { logf("waiting for slot (%d in use)", n) })
	if err != nil {
		if stop := stoppedOutcome(err); stop != nil {
			res.Outcome = *stop
			res.Detail = fmt.Sprintf("%s waiting %s for a slot", res.Outcome, time.Since(waitStart).Round(time.Second))
			return res, nil
		}
		return res, err
	}
	released := false
	release := func() {
		if !released {
			_ = slot.Release()
			released = true
		}
	}
	defer release()

	// Baseline after the slot is ours: edits made while waiting are part of
	// the tree under test, not a mutation by the check.
	base, err := TreeHash(ctx, opts.Root)
	if err != nil {
		return res, err
	}
	res.Tree = base

	logf("running %q", cfg.Gate.Command)
	start := time.Now()
	cr, err := runCheck(ctx, opts.Root, cfg.Gate.Command, slot.File(), out)
	res.Duration = time.Since(start)
	if err != nil {
		return res, err
	}
	res.ExitCode = cr.exitCode

	switch {
	case cr.stopped != nil:
		res.Outcome = *stoppedOutcome(cr.stopped)
		res.Detail = fmt.Sprintf("%s after running %s", res.Outcome, res.Duration.Round(time.Second))
	default:
		// Use a fresh context: the run's deadline may be nearly spent and
		// the guard must still run.
		post, err := TreeHash(context.WithoutCancel(ctx), opts.Root)
		if err != nil {
			return res, err
		}
		switch {
		case post != base:
			res.Outcome = TreeChanged
			res.Changed, _ = changedPaths(context.WithoutCancel(ctx), opts.Root, base, post)
			res.Detail = "the work tree changed during the check (the command wrote files, or something edited the tree concurrently); the gate must only verify"
		case cr.exitCode == 127:
			res.Outcome = ConfigError
			res.Detail = fmt.Sprintf("command not found (shell exit 127) running %q", cfg.Gate.Command)
		case cr.exitCode != 0:
			res.Outcome = Failed
			res.Detail = fmt.Sprintf("check exited %d", cr.exitCode)
		default:
			res.Outcome = Pass
		}
	}
	release()

	wctx := context.WithoutCancel(ctx)
	if res.Outcome == Pass {
		host, _ := os.Hostname()
		err := writeReceipt(wctx, opts.Root, Receipt{
			Tree: base, Command: cfg.Gate.Command, ConfigHash: cfg.Hash(),
			DurationMS: res.Duration.Milliseconds(), At: time.Now().UTC(),
			Host: host, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Version: version,
		})
		if err != nil {
			res.Warning = fmt.Sprintf("PASS, but receipt not recorded (%v)", err)
		}
		return res, nil
	}
	if res.Outcome != Interrupted {
		if err := writeLastRed(state, LastRed{Tree: res.Tree, Kind: res.Outcome.String(), At: time.Now().UTC()}); err != nil {
			res.Warning = fmt.Sprintf("last failure not recorded (%v)", err)
		}
	}
	return res, nil
}

// stoppedOutcome maps a context error to TimedOut or Interrupted, or nil if
// err is not a context error.
func stoppedOutcome(err error) *Outcome {
	var o Outcome
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		o = TimedOut
	case errors.Is(err, context.Canceled):
		o = Interrupted
	default:
		return nil
	}
	return &o
}

func short(tree string) string {
	if len(tree) > 12 {
		return tree[:12]
	}
	return tree
}
