// newproj scaffolds a new Go project in the container layout: a bare
// repository plus a "main" worktree, with the Makefile verb contract,
// lint config, agent rules, CI, and (optionally) a beads database.
package main

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

// The "all:" prefix matters: without it, embed silently skips files whose
// names start with "." or "_" (so .gitignore would never ship).
//
//go:embed all:templates
var templates embed.FS

// defaultPrefix is where your modules live. Override per-run with -prefix
// or permanently with the NEWPROJ_PREFIX environment variable.
const defaultPrefix = "github.com/extremenelson"

const defaultBranch = "main"

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// config is everything the templates and steps need. Exported fields
// (capitalized) are visible to text/template; unexported ones are not.
type config struct {
	Name   string // project and binary name, e.g. "costscan"
	Module string // full module path, e.g. "github.com/you/costscan"
	Dir    string // absolute project directory (the container, unless Flat)
	Flat   bool   // ordinary single-checkout repo instead of a container
	Git    bool
	Beads  bool
}

// workDir is where the project's files live: the main worktree inside the
// container, or the project directory itself for a flat repo.
func (c config) workDir() string {
	if c.Flat {
		return c.Dir
	}
	return filepath.Join(c.Dir, defaultBranch)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "newproj:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := parseArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}

	if _, err := os.Stat(cfg.Dir); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite", cfg.Dir)
	}

	if err := initRepo(cfg); err != nil {
		return err
	}
	work := cfg.workDir()

	if err := render(cfg, work); err != nil {
		return fmt.Errorf("rendering templates: %w", err)
	}
	if err := sh(work, "go", "mod", "init", cfg.Module); err != nil {
		return err
	}
	if cfg.Beads {
		initBeads(work)
	}
	if cfg.Git {
		if err := sh(work, "git", "add", "--all"); err != nil {
			return err
		}
		if err := sh(work, "git", "commit", "--quiet", "-m", "chore: scaffold "+cfg.Name); err != nil {
			return err
		}
	}

	fmt.Printf("\nCreated %s\n  cd %s && make check\n", cfg.Module, work)
	return nil
}

// initRepo creates the directories and the git repository, leaving an
// empty working directory at cfg.workDir() for render to fill.
func initRepo(cfg config) error {
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return err
	}
	if !cfg.Git {
		return nil
	}
	if cfg.Flat {
		return sh(cfg.Dir, "git", "init", "--quiet", "--initial-branch="+defaultBranch)
	}

	// Container layout: <dir>/.bare is the repository, <dir>/.git is a
	// one-line pointer to it, and every checkout is a worktree beside them.
	if err := sh(cfg.Dir, "git", "init", "--quiet", "--bare", "--initial-branch="+defaultBranch, ".bare"); err != nil {
		return err
	}
	pointer := filepath.Join(cfg.Dir, ".git")
	if err := os.WriteFile(pointer, []byte("gitdir: ./.bare\n"), 0o644); err != nil {
		return err
	}
	// --orphan: the repository has no commits yet, so there is nothing to
	// check out. Needs git 2.42 or newer.
	if err := sh(cfg.Dir, "git", "worktree", "add", "--quiet", "--orphan", "-b", defaultBranch, defaultBranch); err != nil {
		return fmt.Errorf("%w (the container layout needs git 2.42+; upgrade git or pass -flat)", err)
	}
	return nil
}

func parseArgs(args []string) (config, error) {
	prefix := defaultPrefix
	if env := os.Getenv("NEWPROJ_PREFIX"); env != "" {
		prefix = env
	}

	fl := flag.NewFlagSet("newproj", flag.ContinueOnError)
	fl.Usage = func() {
		_, _ = fmt.Fprintln(fl.Output(), "usage: newproj [flags] <name>\n\nflags:")
		fl.PrintDefaults()
	}
	modPrefix := fl.String("prefix", prefix, "module path prefix")
	parent := fl.String("dir", ".", "parent directory to create the project in")
	flat := fl.Bool("flat", false, "ordinary single-checkout repo instead of the container layout")
	noGit := fl.Bool("no-git", false, "skip git entirely (implies -flat)")
	beads := fl.Bool("bd", false, "also run 'bd init' (requires bd on PATH)")
	if err := fl.Parse(args); err != nil {
		return config{}, err
	}

	if fl.NArg() != 1 {
		fl.Usage()
		return config{}, errors.New("exactly one project name is required (flags go before the name)")
	}
	name := fl.Arg(0)
	if !validName.MatchString(name) {
		return config{}, fmt.Errorf("invalid name %q: use lowercase letters, digits, and hyphens", name)
	}

	dir, err := filepath.Abs(filepath.Join(*parent, name))
	if err != nil {
		return config{}, err
	}
	return config{
		Name:   name,
		Module: strings.TrimSuffix(*modPrefix, "/") + "/" + name,
		Dir:    dir,
		Flat:   *flat || *noGit,
		Git:    !*noGit,
		Beads:  *beads,
	}, nil
}

// render walks the embedded template tree and writes each file into dest,
// substituting __name__ in paths and {{.Field}} in contents.
func render(cfg config, dest string) error {
	return fs.WalkDir(templates, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "templates")
		rel = strings.ReplaceAll(rel, "__name__", cfg.Name)
		rel = strings.TrimSuffix(rel, ".tmpl")
		target := filepath.Join(dest, filepath.FromSlash(rel))

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		tmpl, err := template.ParseFS(templates, path)
		if err != nil {
			return err
		}
		// Shell scripts must be executable; everything else is a plain file.
		mode := os.FileMode(0o644)
		if strings.HasSuffix(target, ".sh") {
			mode = 0o755
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		// Close explicitly: a failed close can mean the write never landed.
		return errors.Join(tmpl.Execute(out, cfg), out.Close())
	})
}

// initBeads is best-effort: a missing or failing bd should not cost you
// the rest of the scaffold.
func initBeads(dir string) {
	if _, err := exec.LookPath("bd"); err != nil {
		fmt.Fprintln(os.Stderr, "warning: bd not found on PATH; skipping beads init")
		return
	}
	if err := sh(dir, "bd", "init"); err != nil {
		fmt.Fprintln(os.Stderr, "warning: bd init failed; run it by hand:", err)
	}
}

// sh runs a command in dir, streaming its output to the terminal.
func sh(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
