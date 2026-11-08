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

	"github.com/extremenelson/newproj/internal/gate"
)

// runGate is `newproj gate`: prove the current work tree passes its
// configured check. Exit status: 0 pass (or cached), 1 check failed,
// 2 config error, 3 tree changed during the check, 4 timed out,
// 130 interrupted.
func runGate(args []string) error {
	fl := flag.NewFlagSet("newproj gate", flag.ContinueOnError)
	fl.Usage = func() {
		_, _ = fmt.Fprintln(fl.Output(), "usage: newproj gate [flags]\n       newproj gate --status [<rev>]\n\n"+
			"Runs the check in .newproj.json and records a receipt when it passes.\n"+
			"Exit status: 0 pass, 1 failed, 2 config error, 3 tree changed, 4 timed out.\n"+
			"With --status: 0 proven, 1 not proven, 2 error; never runs the check.\n\nflags:")
		fl.PrintDefaults()
	}
	budget := fl.Duration("budget", 0, "give up after this long, including the wait for a slot (0: no limit)")
	noCache := fl.Bool("no-cache", false, "run the check even if a receipt already proves this tree")
	status := fl.Bool("status", false, "report whether the working tree (or <rev>) is proven, without running the check")
	hook := fl.Bool("hook", false, "run as a Claude Code Stop hook: read hook JSON on stdin, print the decision as JSON, exit 0")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return exitCode(gate.ConfigError.ExitCode())
	}
	if fl.NArg() > 1 || (fl.NArg() == 1 && !*status) {
		fl.Usage()
		return exitCode(gate.ConfigError.ExitCode())
	}
	if *status && *hook {
		fmt.Fprintln(os.Stderr, "gate: --status and --hook cannot be combined")
		return exitCode(gate.ConfigError.ExitCode())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *budget)
		defer cancel()
	}

	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	if *hook {
		if *noCache {
			fmt.Fprintln(os.Stderr, "gate: --no-cache is not supported with --hook")
		}
		resp := gate.Hook(ctx, os.Stdin, gate.HookOptions{FallbackDir: wd})
		return json.NewEncoder(os.Stdout).Encode(resp)
	}
	root, err := gate.TopLevel(ctx, wd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gate:", err)
		return exitCode(gate.ConfigError.ExitCode())
	}

	if *status {
		return runStatus(ctx, root, fl.Arg(0))
	}

	res, err := gate.Run(ctx, gate.Options{
		Root: root, NoCache: *noCache, Out: os.Stdout, Log: os.Stderr,
	})
	if err != nil {
		return err
	}
	report(res)
	return exitCodeOf(res.Outcome)
}

// report prints the run's one-line verdict (and details) to stderr.
func report(res gate.Result) {
	switch res.Outcome {
	case gate.Pass:
		fmt.Fprintf(os.Stderr, "gate: PASS (tree %s, %s)\n", shortTree(res.Tree), res.Duration.Round(time.Millisecond))
	case gate.Cached:
		// Run already logged the cached pass.
	default:
		fmt.Fprintf(os.Stderr, "gate: %s: %s\n", strings.ToUpper(res.Outcome.String()), res.Detail)
		for _, p := range res.Changed {
			fmt.Fprintln(os.Stderr, "  changed:", p)
		}
	}
	if res.Warning != "" {
		fmt.Fprintln(os.Stderr, "gate: warning:", res.Warning)
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

// runStatus is `newproj gate --status [<rev>]`: exit 0 when the tree is
// proven, 1 when it is not, 2 when the status cannot be determined.
func runStatus(ctx context.Context, root, rev string) error {
	rep, err := gate.Status(ctx, gate.StatusOptions{Root: root, Rev: rev})
	if err != nil {
		fmt.Fprintln(os.Stderr, "gate:", err)
		return exitCode(2)
	}
	fmt.Printf("%s (tree %s)\n", rep.Target, shortTree(rep.Tree))
	switch {
	case rep.Proven:
		r := rep.Receipt
		fmt.Printf("  PASS %s ago: %q in %s on %s (newproj %s)\n",
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
