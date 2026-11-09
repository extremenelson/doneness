// doneness makes repositories safe for parallel Claude Code agents: an
// enforced definition of done. `doneness adopt` retrofits a repository,
// `doneness check` runs its check, `doneness status` reports whether a tree
// is proven, `doneness hook` is the Claude Code Stop hook, and `doneness new`
// scaffolds a Go project with all of it wired in.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		var ec exitCode
		if errors.As(err, &ec) {
			os.Exit(int(ec))
		}
		fmt.Fprintln(os.Stderr, "doneness:", err)
		os.Exit(1)
	}
}

// exitCode is an error that carries a specific process exit status; the
// command has already reported what happened.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

const usage = `usage: doneness <command> [flags]

commands:
  adopt          add the done-gate to this repository (dry run unless --apply)
  check          run this repository's check and record a receipt when it passes
  status [<rev>] is the working tree (or <rev>) proven? never runs the check
  hook           the Claude Code Stop hook (installed by adopt and new)
  new <name>     scaffold a new Go project with the gate wired in

Run doneness <command> -h for a command's flags.
`

// run routes to a subcommand.
func run(args []string) error {
	if len(args) == 0 {
		_, _ = io.WriteString(os.Stderr, usage)
		return exitCode(2)
	}
	switch args[0] {
	case "adopt":
		return runAdopt(args[1:])
	case "check":
		return runCheck(args[1:])
	case "status":
		return runStatus(args[1:])
	case "hook":
		return runHook(args[1:])
	case "new":
		return runNew(args[1:])
	case "help", "-h", "-help", "--help":
		_, _ = io.WriteString(os.Stdout, usage)
		return nil
	}
	_, _ = fmt.Fprintf(os.Stderr, "doneness: unknown command %q\n\n%s", args[0], usage)
	return exitCode(2)
}
