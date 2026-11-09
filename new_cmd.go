package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/extremenelson/doneness/internal/scaffold"
)

// prefixEnv sets the module path prefix when -prefix is not given.
const prefixEnv = "DONENESS_PREFIX"

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// runNew is `doneness new [flags] <name>`.
func runNew(args []string) error {
	cfg, err := parseArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := scaffold.Create(cfg, os.Stderr); err != nil {
		return err
	}
	fmt.Printf("\nCreated %s\n  cd %s && make check\n", cfg.Module, cfg.WorkDir())
	return nil
}

func parseArgs(args []string) (scaffold.Config, error) {
	fl := flag.NewFlagSet("doneness new", flag.ContinueOnError)
	fl.Usage = func() {
		_, _ = fmt.Fprintln(fl.Output(), "usage: doneness new [flags] <name>\n\nflags:")
		fl.PrintDefaults()
	}
	modPrefix := fl.String("prefix", "", "module path prefix (default: $"+prefixEnv+", else github.com/<git config github.user>)")
	parent := fl.String("dir", ".", "parent directory to create the project in")
	flat := fl.Bool("flat", false, "ordinary single-checkout repo instead of the container layout")
	noGit := fl.Bool("no-git", false, "skip git entirely (implies -flat)")
	beads := fl.Bool("bd", false, "also run 'bd init' (requires bd on PATH)")
	if err := fl.Parse(args); err != nil {
		return scaffold.Config{}, err
	}

	if fl.NArg() != 1 {
		fl.Usage()
		return scaffold.Config{}, errors.New("exactly one project name is required (flags go before the name)")
	}
	name := fl.Arg(0)
	if !validName.MatchString(name) {
		return scaffold.Config{}, fmt.Errorf("invalid name %q: use lowercase letters, digits, and hyphens", name)
	}

	prefix, err := modulePrefix(*modPrefix)
	if err != nil {
		return scaffold.Config{}, err
	}
	dir, err := filepath.Abs(filepath.Join(*parent, name))
	if err != nil {
		return scaffold.Config{}, err
	}
	return scaffold.Config{
		Name:   name,
		Module: strings.TrimSuffix(prefix, "/") + "/" + name,
		Dir:    dir,
		Flat:   *flat || *noGit,
		Git:    !*noGit,
		Beads:  *beads,
	}, nil
}

// modulePrefix picks the module path prefix: the -prefix flag, else
// $DONENESS_PREFIX, else github.com/<git config github.user>.
func modulePrefix(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if env := os.Getenv(prefixEnv); env != "" {
		return env, nil
	}
	out, err := exec.Command("git", "config", "--get", "github.user").Output()
	if user := strings.TrimSpace(string(out)); err == nil && user != "" {
		return "github.com/" + user, nil
	}
	return "", fmt.Errorf("no module path prefix: pass -prefix (e.g. -prefix github.com/you), set $%s, or run `git config --global github.user <you>`", prefixEnv)
}
