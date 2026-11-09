package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/extremenelson/doneness/internal/gate"
)

// newFlags returns a flag set whose -h output starts with usage.
func newFlags(name, usage string) *flag.FlagSet {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.Usage = func() {
		_, _ = fmt.Fprintln(fl.Output(), usage)
		fl.PrintDefaults()
	}
	return fl
}

// parse parses args and maps a usage error to exit 2 (help to nil). It
// allows at most maxArgs positional arguments.
func parse(fl *flag.FlagSet, args []string, maxArgs int) (done bool, err error) {
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, nil
		}
		return true, exitCode(gate.ConfigError.ExitCode())
	}
	if fl.NArg() > maxArgs {
		fl.Usage()
		return true, exitCode(gate.ConfigError.ExitCode())
	}
	return false, nil
}

// signalContext is cancelled on Ctrl-C or SIGTERM and, when budget > 0,
// after budget.
func signalContext(budget time.Duration) (context.Context, func()) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if budget <= 0 {
		return ctx, stop
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	return ctx, func() { cancel(); stop() }
}

// repoRoot is the top level of the work tree containing the current
// directory; outside a repository it reports and returns exit 2.
func repoRoot(ctx context.Context) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := gate.TopLevel(ctx, wd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "doneness:", err)
		return "", exitCode(gate.ConfigError.ExitCode())
	}
	return root, nil
}

// runCheck is `doneness check`: prove the current work tree passes its
// configured check. Exit status: 0 pass (or cached), 1 check failed,
// 2 config error, 3 tree changed during the check, 4 timed out,
// 130 interrupted.
func runCheck(args []string) error {
	fl := newFlags("doneness check", "usage: doneness check [flags]\n\n"+
		"Runs the check in "+gate.ConfigFile+" and records a receipt when it passes.\n"+
		"Exit status: 0 pass, 1 failed, 2 config error, 3 tree changed, 4 timed out.\n\nflags:")
	budget := fl.Duration("budget", 0, "give up after this long, including the wait for a slot (0: no limit)")
	noCache := fl.Bool("no-cache", false, "run the check even if a receipt already proves this tree")
	if done, err := parse(fl, args, 0); done {
		return err
	}
	ctx, stop := signalContext(*budget)
	defer stop()
	root, err := repoRoot(ctx)
	if err != nil {
		return err
	}
	res, err := gate.Run(ctx, gate.Options{Root: root, NoCache: *noCache, Out: os.Stdout, Log: os.Stderr})
	if err != nil {
		return err
	}
	report(res)
	return exitCodeOf(res.Outcome)
}

// runStatus is `doneness status [<rev>]`: exit 0 when the tree is proven,
// 1 when it is not, 2 when the status cannot be determined. It never runs
// the check.
func runStatus(args []string) error {
	fl := newFlags("doneness status", "usage: doneness status [<rev>]\n\n"+
		"Reports whether the working tree (or <rev>) is proven by a receipt, and warns\n"+
		"about gate setup problems. Never runs the check.\n"+
		"Exit status: 0 proven, 1 not proven, 2 error.")
	if done, err := parse(fl, args, 1); done {
		return err
	}
	ctx, stop := signalContext(0)
	defer stop()
	root, err := repoRoot(ctx)
	if err != nil {
		return err
	}
	rep, err := gate.Status(ctx, gate.StatusOptions{Root: root, Rev: fl.Arg(0)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "doneness:", err)
		return exitCode(2)
	}
	fmt.Printf("%s (tree %s)\n", rep.Target, shortTree(rep.Tree))
	switch {
	case rep.Proven:
		r := rep.Receipt
		fmt.Printf("  PASS %s ago: %q in %s on %s (doneness %s)\n",
			time.Since(r.At).Round(time.Second), r.Command,
			(time.Duration(r.DurationMS) * time.Millisecond).Round(time.Millisecond), r.Host, r.Version)
	case rep.Stale != "":
		fmt.Printf("  unproven: stale receipt (%s)\n", rep.Stale)
	case rep.LastRed != nil:
		fmt.Printf("  unproven (last attempt RED: %s, %s ago)\n", rep.LastRed.Kind, time.Since(rep.LastRed.At).Round(time.Second))
	default:
		fmt.Println("  unproven")
	}
	for _, p := range rep.Problems {
		fmt.Println("warning:", p)
	}
	if !rep.Proven {
		return exitCode(1)
	}
	return nil
}

// runHook is `doneness hook`: the Claude Code Stop hook. It reads the hook
// JSON on stdin, gates the session's repository, prints the decision as
// JSON and exits 0.
func runHook(args []string) error {
	fl := newFlags("doneness hook", "usage: doneness hook [flags]\n\n"+
		"Claude Code Stop hook (installed by `doneness adopt` and `doneness new`).\n"+
		"Reads hook JSON on stdin, prints the decision as JSON, always exits 0.\n\nflags:")
	budget := fl.Duration("budget", 0, "give up after this long, including the wait for a slot (0: no limit)")
	if done, err := parse(fl, args, 0); done {
		return err
	}
	ctx, stop := signalContext(*budget)
	defer stop()
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	resp := gate.Hook(ctx, os.Stdin, gate.HookOptions{FallbackDir: wd})
	return json.NewEncoder(os.Stdout).Encode(resp)
}

// report prints the run's one-line verdict (and details) to stderr.
func report(res gate.Result) {
	switch res.Outcome {
	case gate.Pass:
		fmt.Fprintf(os.Stderr, "doneness: PASS (tree %s, %s)\n", shortTree(res.Tree), res.Duration.Round(time.Millisecond))
	case gate.Cached:
		// Run already logged the cached pass.
	default:
		fmt.Fprintf(os.Stderr, "doneness: %s: %s\n", strings.ToUpper(res.Outcome.String()), res.Detail)
		for _, p := range res.Changed {
			fmt.Fprintln(os.Stderr, "  changed:", p)
		}
	}
	if res.Warning != "" {
		fmt.Fprintln(os.Stderr, "doneness: warning:", res.Warning)
	}
}

func exitCodeOf(o gate.Outcome) error {
	if c := o.ExitCode(); c != 0 {
		return exitCode(c)
	}
	return nil
}

func shortTree(t string) string {
	if len(t) > 12 {
		return t[:12]
	}
	return t
}
