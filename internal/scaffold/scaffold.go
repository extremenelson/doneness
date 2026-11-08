// Package scaffold creates a new Go project from the embedded templates: a
// container layout (a bare repository plus a "main" worktree) or a flat
// repository, with the Makefile verb contract, lint config, agent rules, CI,
// the done-gate config and its Claude Code Stop hook, and optionally a beads
// database.
package scaffold

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/extremenelson/newproj/internal/gate"
)

// The "all:" prefix matters: without it, embed silently skips files whose
// names start with "." or "_" (so .gitignore would never ship).
//
//go:embed all:templates
var templates embed.FS

// DefaultBranch is the scaffolded repository's initial branch, and the name
// of the main worktree in the container layout.
const DefaultBranch = "main"

// GateCommand is the check a scaffolded project's gate runs.
const GateCommand = "make check"

// HookTimeout is the Stop-hook timeout written into the scaffold's
// committed .claude/settings.json.
const HookTimeout = 1800 * time.Second

// Config is everything the templates and steps need. Exported fields are
// visible to text/template.
type Config struct {
	Name   string // project and binary name, e.g. "costscan"
	Module string // full module path, e.g. "github.com/you/costscan"
	Dir    string // absolute project directory (the container, unless Flat)
	Flat   bool   // ordinary single-checkout repo instead of a container
	Git    bool
	Beads  bool

	// HookCommandJSON is the Stop-hook command as a JSON string literal
	// (quotes included), set by Create for the settings template.
	HookCommandJSON string
	// HookTimeoutSeconds is HookTimeout in seconds, for the settings template.
	HookTimeoutSeconds int
}

// WorkDir is where the project's files live: the main worktree inside the
// container, or the project directory itself for a flat repo.
func (c Config) WorkDir() string {
	if c.Flat {
		return c.Dir
	}
	return filepath.Join(c.Dir, DefaultBranch)
}

// Create scaffolds the project described by cfg, streaming command output
// to out. It refuses to touch an existing directory.
func Create(cfg Config, out io.Writer) error {
	if _, err := os.Stat(cfg.Dir); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite", cfg.Dir)
	}

	// The scaffold's settings.json is committed and shared, so the hook
	// looks the gate up on PATH rather than naming this machine's binary.
	hook, err := gate.HookCommand("", HookTimeout)
	if err != nil {
		return err
	}
	quoted, err := gate.EncodeJSON(hook, false)
	if err != nil {
		return err
	}
	cfg.HookCommandJSON = string(quoted)
	cfg.HookTimeoutSeconds = int(HookTimeout / time.Second)

	if err := initRepo(cfg, out); err != nil {
		return err
	}
	work := cfg.WorkDir()

	if err := render(templates, cfg, work); err != nil {
		return fmt.Errorf("rendering templates: %w", err)
	}
	if err := gate.WriteConfig(work, gate.Config{Gate: gate.Gate{Command: GateCommand}}); err != nil {
		return fmt.Errorf("writing gate config: %w", err)
	}
	if err := sh(out, work, "go", "mod", "init", cfg.Module); err != nil {
		return err
	}
	if cfg.Beads {
		initBeads(out, work)
	}
	if cfg.Git {
		if err := sh(out, work, "git", "add", "--all"); err != nil {
			return err
		}
		if err := sh(out, work, "git", "commit", "--quiet", "-m", "chore: scaffold "+cfg.Name); err != nil {
			return err
		}
	}
	return nil
}

// initRepo creates the directories and the git repository, leaving an
// empty working directory at cfg.WorkDir() for render to fill.
func initRepo(cfg Config, out io.Writer) error {
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return err
	}
	if !cfg.Git {
		return nil
	}
	if cfg.Flat {
		return sh(out, cfg.Dir, "git", "init", "--quiet", "--initial-branch="+DefaultBranch)
	}

	// Container layout: <dir>/.bare is the repository, <dir>/.git is a
	// one-line pointer to it, and every checkout is a worktree beside them.
	if err := sh(out, cfg.Dir, "git", "init", "--quiet", "--bare", "--initial-branch="+DefaultBranch, ".bare"); err != nil {
		return err
	}
	pointer := filepath.Join(cfg.Dir, ".git")
	if err := os.WriteFile(pointer, []byte("gitdir: ./.bare\n"), 0o644); err != nil {
		return err
	}
	// --orphan: the repository has no commits yet, so there is nothing to
	// check out. Needs git 2.42 or newer.
	if err := sh(out, cfg.Dir, "git", "worktree", "add", "--quiet", "--orphan", "-b", DefaultBranch, DefaultBranch); err != nil {
		return fmt.Errorf("%w (the container layout needs git 2.42+; upgrade git or pass -flat)", err)
	}
	return nil
}

// render walks the template tree in fsys (rooted at "templates") and writes
// each file into dest, substituting __name__ in paths and {{.Field}} in
// contents. Files ending in .sh are made executable.
func render(fsys fs.FS, cfg Config, dest string) error {
	return fs.WalkDir(fsys, "templates", func(path string, d fs.DirEntry, err error) error {
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

		tmpl, err := template.ParseFS(fsys, path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(target, ".sh") {
			mode = 0o755
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		// Close explicitly: a failed close can mean the write never landed.
		return errors.Join(tmpl.Execute(f, cfg), f.Close())
	})
}

// initBeads is best-effort: a missing or failing bd should not cost you
// the rest of the scaffold.
func initBeads(out io.Writer, dir string) {
	if _, err := exec.LookPath("bd"); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "warning: bd not found on PATH; skipping beads init")
		return
	}
	if err := sh(out, dir, "bd", "init"); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "warning: bd init failed; run it by hand:", err)
	}
}

// sh runs a command in dir, streaming its output to out.
func sh(out io.Writer, dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
