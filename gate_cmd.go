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
		_, _ = fmt.Fprintln(fl.Output(), "usage: newproj gate [flags]\n\n"+
			"Runs the check in .newproj.json and records a receipt when it passes.\n"+
			"Exit status: 0 pass, 1 failed, 2 config error, 3 tree changed, 4 timed out.\n\nflags:")
		fl.PrintDefaults()
	}
	budget := fl.Duration("budget", 0, "give up after this long, including the wait for a slot (0: no limit)")
	noCache := fl.Bool("no-cache", false, "run the check even if a receipt already proves this tree")
	hook := fl.Bool("hook", false, "run as a Claude Code Stop hook: read hook JSON on stdin, print the decision as JSON, exit 0")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return exitCode(gate.ConfigError.ExitCode())
	}
	if fl.NArg() != 0 {
		fl.Usage()
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
