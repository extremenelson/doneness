// newproj makes repositories safe for parallel coding agents: `newproj new`
// scaffolds a Go project with an enforced definition of done, and
// `newproj gate` runs that check (as a command or a Claude Code Stop hook).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		var ec exitCode
		if errors.As(err, &ec) {
			os.Exit(int(ec))
		}
		fmt.Fprintln(os.Stderr, "newproj:", err)
		os.Exit(1)
	}
}

// exitCode is an error that carries a specific process exit status; the
// command has already reported what happened.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

const usage = `usage: newproj <command> [flags]

commands:
  new <name>   scaffold a new Go project (newproj new -h for flags)
  gate         run this repository's done-gate (newproj gate -h for flags)
`

// run routes to a subcommand. A first argument that is not a command is
// the pre-subcommand form `newproj [flags] <name>`, kept for one release as
// an alias for `newproj new`.
func run(args []string) error {
	if len(args) == 0 {
		_, _ = io.WriteString(os.Stderr, usage)
		return exitCode(2)
	}
	switch args[0] {
	case "new":
		return runNew(args[1:])
	case "gate":
		return runGate(args[1:])
	case "help", "-h", "-help", "--help":
		_, _ = io.WriteString(os.Stdout, usage)
		return nil
	}
	if !strings.HasPrefix(args[0], "-") && !validName.MatchString(args[0]) {
		_, _ = fmt.Fprintf(os.Stderr, "newproj: unknown command %q\n\n%s", args[0], usage)
		return exitCode(2)
	}
	_, _ = fmt.Fprintln(os.Stderr, "newproj: `newproj [flags] <name>` is deprecated; use `newproj new [flags] <name>`")
	return runNew(args)
}
