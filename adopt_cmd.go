package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/extremenelson/doneness/internal/adopt"
	"github.com/extremenelson/doneness/internal/gate"
)

// runAdopt is `doneness adopt`: retrofit the current repository with the
// done-gate. It prints a plan (dry run) unless --apply is given. Exit status:
// 0 dry run or READY, 1 NOT READY (applied, but the gate is red), 2 usage or
// setup error.
func runAdopt(args []string) error {
	fl := flag.NewFlagSet("doneness adopt", flag.ContinueOnError)
	fl.Usage = func() {
		_, _ = fmt.Fprintln(fl.Output(), "usage: doneness adopt [flags]\n\n"+
			"Detects this repository's check command and shows what adopting the done-gate\n"+
			"would change. Nothing is written unless --apply is given; nothing is committed.\n"+
			"Exit status: 0 plan shown or READY, 1 NOT READY, 2 error.\n\nflags:")
		fl.PrintDefaults()
	}
	apply := fl.Bool("apply", false, "write the changes, then run the gate once")
	cmd := fl.String("cmd", "", "gate command to use instead of the detected one")
	agentsFile := fl.String("agents-file", "", "file for the agent rules block (default: CLAUDE.md, else AGENTS.md, else a new CLAUDE.md)")
	shared := fl.Bool("shared", false, "install the hook in the committed .claude/settings.json (default: your .claude/settings.local.json)")
	hookTimeout := fl.Duration("hook-timeout", 1800*time.Second, "Stop-hook timeout (minimum 60s); the gate stops 30s before it")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return exitCode(2)
	}
	if fl.NArg() != 0 {
		fl.Usage()
		return exitCode(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fail := func(err error) error {
		fmt.Fprintln(os.Stderr, "adopt:", err)
		return exitCode(2)
	}

	wd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	root, err := gate.TopLevel(ctx, wd)
	if err != nil {
		return fail(err)
	}
	bin, err := os.Executable()
	if err != nil && !*shared {
		return fail(fmt.Errorf("locate this binary: %w", err))
	}

	plan, err := adopt.Build(ctx, adopt.Options{
		Root: root, Command: *cmd, AgentsFile: *agentsFile, Shared: *shared,
		HookTimeout: *hookTimeout, Binary: bin,
	})
	if err != nil {
		return fail(err)
	}
	if err := plan.Write(ctx, os.Stdout); err != nil {
		return fail(err)
	}
	if !*apply {
		fmt.Println("\ndry run: nothing was written. Re-run with --apply to make these changes.")
		return nil
	}

	if err := plan.Apply(); err != nil {
		return fail(err)
	}
	fmt.Println("\napplied. Running the gate once:")
	res, err := gate.Run(ctx, gate.Options{Root: root, Out: os.Stdout, Log: os.Stderr})
	if err != nil {
		return fail(err)
	}
	if !res.Outcome.Passed() {
		fmt.Printf("\nNOT READY: gate is %s: %s\n", res.Outcome, res.Detail)
		fmt.Println("The files stay written: fix the check (or pass --cmd) and run `doneness check`.")
		return exitCode(1)
	}
	toCommit := []string{gate.ConfigFile}
	for _, c := range plan.Changes {
		if c.Rel != gate.ConfigFile && !strings.HasSuffix(c.Path, "settings.local.json") {
			toCommit = append(toCommit, c.Rel)
		}
	}
	fmt.Printf("\nREADY: commit %s so new worktrees inherit the gate.\n", strings.Join(toCommit, ", "))
	return nil
}
