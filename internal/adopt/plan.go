package adopt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/extremenelson/newproj/internal/gate"
)

// Options configures Build.
type Options struct {
	Root        string        // work tree top level
	Command     string        // --cmd: the gate command; "" to keep or detect
	AgentsFile  string        // --agents-file: rules file; "" for CLAUDE.md, else AGENTS.md, else new CLAUDE.md
	Shared      bool          // --shared: committed .claude/settings.json instead of local settings
	HookTimeout time.Duration // --hook-timeout
	Binary      string        // absolute path of this gate binary, for the local hook
}

// Change is one file adopt writes.
type Change struct {
	Path string // absolute
	Rel  string // for display
	Old  []byte // nil when the file does not exist
	New  []byte
	Note string
}

// Plan is everything adopt would do, computed without touching the repo.
type Plan struct {
	Detection     Detection
	Command       string
	CommandSource string
	HookFile      string // where the Stop hook goes
	Changes       []Change
	SideEffects   []string
	Warnings      []string
	exclude       *excludeEdit
}

type excludeEdit struct {
	path, line string
}

// The rules block is delimited so a re-run replaces it instead of adding
// another copy.
const (
	blockBegin = "<!-- newproj:begin (managed by `newproj adopt`; edits inside are replaced) -->"
	blockEnd   = "<!-- newproj:end -->"
)

// Build computes what adopt would change in the repository at opts.Root.
func Build(ctx context.Context, opts Options) (*Plan, error) {
	if opts.HookTimeout < gate.MinHookTimeout {
		return nil, fmt.Errorf("--hook-timeout %v is below the %v minimum", opts.HookTimeout, gate.MinHookTimeout)
	}
	p := &Plan{}
	det, err := Detect(opts.Root)
	if err != nil {
		return nil, err
	}
	p.Detection = det

	// The gate command: --cmd, else an existing config (a re-run keeps
	// your choice), else the best detected candidate.
	existing, cfgErr := gate.LoadConfig(opts.Root)
	switch {
	case opts.Command != "":
		p.Command, p.CommandSource = opts.Command, "--cmd"
	case cfgErr == nil:
		p.Command, p.CommandSource = existing.Gate.Command, "existing "+gate.ConfigFile
	case !errors.Is(cfgErr, gate.ErrNoConfig):
		return nil, fmt.Errorf("existing %s: %w (fix it or pass --cmd)", gate.ConfigFile, cfgErr)
	case len(det.Candidates) > 0:
		p.Command, p.CommandSource = det.Candidates[0].Command, det.Candidates[0].Source
	default:
		return nil, errors.New("no check command found (no Makefile, justfile, package.json, Cargo.toml or go.mod targets); pass --cmd \"<command>\"")
	}

	if err := p.addConfig(opts.Root); err != nil {
		return nil, err
	}
	if err := p.addHook(ctx, opts); err != nil {
		return nil, err
	}
	if err := p.addRules(opts); err != nil {
		return nil, err
	}

	p.SideEffects = append(p.SideEffects,
		"--apply runs `newproj gate` once: a pass records a receipt in "+gate.NotesRef+" (never pushed)",
		"gate state (slot locks, session counters, last failures) lives under $XDG_STATE_HOME/newproj (default ~/.local/state/newproj)",
		"nothing is committed, moved or pushed, and CI files are not touched",
	)
	return p, nil
}

func (p *Plan) addConfig(root string) error {
	path := filepath.Join(root, gate.ConfigFile)
	old, err := readOptional(path)
	if err != nil {
		return err
	}
	cfg := gate.Config{Gate: gate.Gate{Command: p.Command}}
	if err := cfg.Validate(); err != nil {
		return err
	}
	// Render exactly what gate.WriteConfig writes, for the diff.
	tmp, err := os.MkdirTemp("", "newproj-adopt-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := gate.WriteConfig(tmp, cfg); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(tmp, gate.ConfigFile))
	if err != nil {
		return err
	}
	p.add(Change{Path: path, Rel: gate.ConfigFile, Old: old, New: b})
	return nil
}

func (p *Plan) addHook(ctx context.Context, opts Options) error {
	var dir, file, bin string
	if opts.Shared {
		dir, file, bin = opts.Root, "settings.json", ""
	} else {
		// Claude Code reads a worktree's local settings from the main
		// checkout, so the local hook goes there.
		main, err := gate.MainCheckout(ctx, opts.Root)
		if err != nil {
			return err
		}
		dir, file, bin = main, "settings.local.json", opts.Binary
		if bin == "" || !filepath.IsAbs(bin) {
			return errors.New("cannot determine this binary's absolute path for the local hook; use --shared or install newproj")
		}
		if isTemporary(bin) {
			p.Warnings = append(p.Warnings, fmt.Sprintf("the hook will call %s, which looks like a temporary build (go run?); install newproj (go install) and re-run adopt", bin))
		}
	}
	cmd, err := gate.HookCommand(bin, opts.HookTimeout)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, ".claude", file)
	old, err := readOptional(path)
	if err != nil {
		return err
	}
	h := hookEntry{Type: "command", Command: cmd, Timeout: int(opts.HookTimeout / time.Second)}
	merged, note, err := mergeSettings(old, h, opts.Shared && old != nil)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	p.HookFile = path
	p.add(Change{Path: path, Rel: rel(opts.Root, path), Old: old, New: merged, Note: note})

	if !opts.Shared {
		ignored, err := isIgnored(ctx, dir, ".claude/settings.local.json")
		if err != nil {
			return err
		}
		if !ignored {
			exclude, err := gitOut(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
			if err != nil {
				return err
			}
			p.exclude = &excludeEdit{path: exclude, line: "/.claude/settings.local.json"}
			p.SideEffects = append(p.SideEffects, fmt.Sprintf("append %s to %s so the personal hook is never committed", p.exclude.line, exclude))
		}
	}
	return nil
}

func (p *Plan) addRules(opts Options) error {
	name := opts.AgentsFile
	if name == "" {
		name = "CLAUDE.md"
		if !exists(filepath.Join(opts.Root, "CLAUDE.md")) && exists(filepath.Join(opts.Root, "AGENTS.md")) {
			name = "AGENTS.md"
		}
	}
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(opts.Root, name)
	}
	old, err := readOptional(path)
	if err != nil {
		return err
	}
	p.add(Change{Path: path, Rel: rel(opts.Root, path), Old: old, New: withRulesBlock(old, rulesBlock(p.Command))})
	return nil
}

// rulesBlock is the agent-facing definition of done.
func rulesBlock(command string) string {
	var b strings.Builder
	b.WriteString(blockBegin + "\n")
	b.WriteString("## Definition of done\n\n")
	fmt.Fprintf(&b, "- Done means `newproj gate` passes. It runs `%s` (from `%s`) and records a receipt for the exact tree it tested.\n", command, gate.ConfigFile)
	b.WriteString("- A Stop hook runs the gate when you finish. While it is red you are sent back with the failure; after 3 red attempts the session ends marked NOT DONE.\n")
	b.WriteString("- The check only verifies. If it fails on formatting or generated files, fix them; do not weaken the check.\n")
	fmt.Fprintf(&b, "- Never edit `%s`, the Stop hook or this block to get past the gate.\n", gate.ConfigFile)
	b.WriteString("- One task per git worktree. Before you say a branch is done, `newproj gate --status` must say PASS.\n")
	if _, err := exec.LookPath("bd"); err == nil {
		b.WriteString("- Issues live in beads: run `bd prime` at session start; one bead per branch.\n")
	}
	b.WriteString(blockEnd + "\n")
	return b.String()
}

// withRulesBlock replaces an existing managed block, or appends one.
func withRulesBlock(old []byte, block string) []byte {
	s := string(old)
	if i := strings.Index(s, "<!-- newproj:begin"); i >= 0 {
		if j := strings.Index(s[i:], blockEnd); j >= 0 {
			end := i + j + len(blockEnd)
			if end < len(s) && s[end] == '\n' {
				end++
			}
			return []byte(s[:i] + block + s[end:])
		}
	}
	switch {
	case s == "":
	case strings.HasSuffix(s, "\n\n"):
	case strings.HasSuffix(s, "\n"):
		s += "\n"
	default:
		s += "\n\n"
	}
	return []byte(s + block)
}

// add records a change unless it would leave the file as it is.
func (p *Plan) add(c Change) {
	if c.Old != nil && bytes.Equal(c.Old, c.New) {
		return
	}
	p.Changes = append(p.Changes, c)
}

// Write prints the plan: the chosen command, the alternatives, a unified
// diff of every file, and the side effects of --apply.
func (p *Plan) Write(ctx context.Context, w io.Writer) error {
	// Build the whole plan first (strings.Builder writes cannot fail), then
	// write it once and report that write's error.
	var b strings.Builder
	fmt.Fprintf(&b, "gate command: %s\n  (from %s)\n", p.Command, p.CommandSource)
	for _, c := range p.Detection.Candidates {
		if c.Command != p.Command {
			fmt.Fprintf(&b, "  other candidate: %s  (%s)\n", c.Command, c.Source)
		}
	}
	for _, e := range p.Detection.Excluded {
		fmt.Fprintf(&b, "  %s: %s\n", e.Name, e.Reason)
	}
	b.WriteString("\n")
	if len(p.Changes) == 0 {
		b.WriteString("files: no changes (already adopted)\n")
	}
	for _, c := range p.Changes {
		d, err := unifiedDiff(ctx, c)
		if err != nil {
			return err
		}
		if c.Note != "" {
			fmt.Fprintf(&b, "# %s: %s\n", c.Rel, c.Note)
		}
		b.WriteString(d)
	}
	if len(p.Changes) > 0 {
		b.WriteString("\n")
	}
	for _, s := range p.SideEffects {
		fmt.Fprintln(&b, "side effect:", s)
	}
	for _, s := range p.Warnings {
		fmt.Fprintln(&b, "warning:", s)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Apply writes every change and the exclude entry. Files are replaced
// atomically and keep their existing permissions.
func (p *Plan) Apply() error {
	for _, c := range p.Changes {
		if err := writeFile(c.Path, c.New); err != nil {
			return err
		}
	}
	if p.exclude != nil {
		old, err := readOptional(p.exclude.path)
		if err != nil {
			return err
		}
		if !bytes.Contains(old, []byte(p.exclude.line+"\n")) {
			if len(old) > 0 && !bytes.HasSuffix(old, []byte("\n")) {
				old = append(old, '\n')
			}
			if err := writeFile(p.exclude.path, append(old, p.exclude.line+"\n"...)); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeFile(path string, b []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// unifiedDiff renders a change with git's diff (git is already required).
func unifiedDiff(ctx context.Context, c Change) (string, error) {
	tmp, err := os.MkdirTemp("", "newproj-diff-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	oldPath, newPath := "/dev/null", filepath.Join(tmp, "new")
	if c.Old != nil {
		oldPath = filepath.Join(tmp, "old")
		if err := os.WriteFile(oldPath, c.Old, 0o644); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(newPath, c.New, 0o644); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--no-index", "--no-color", "--no-ext-diff", "--", oldPath, newPath)
	out, err := cmd.Output()
	var ee *exec.ExitError
	if err != nil && (!errors.As(err, &ee) || ee.ExitCode() != 1) { // 1: files differ
		return "", fmt.Errorf("git diff: %w", err)
	}
	s := string(out)
	label := "b/" + c.Rel
	s = strings.ReplaceAll(s, "a"+oldPath, "a/"+c.Rel)
	s = strings.ReplaceAll(s, "b"+newPath, label)
	s = strings.ReplaceAll(s, "a"+newPath, "a/"+c.Rel)
	if lines := strings.SplitN(s, "\n", 2); len(lines) == 2 && strings.HasPrefix(lines[0], "diff --git") {
		s = "diff --git a/" + c.Rel + " " + label + "\n" + lines[1]
	}
	return s, nil
}

func readOptional(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func isIgnored(ctx context.Context, dir, path string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "check-ignore", "-q", "--", path)
	cmd.Dir = dir
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return false, nil
	default:
		return false, fmt.Errorf("git check-ignore: %w", err)
	}
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return path
}

// isTemporary reports binaries built by `go run` or living in a temp dir.
func isTemporary(bin string) bool {
	return strings.Contains(bin, string(filepath.Separator)+"go-build") ||
		strings.HasPrefix(bin, filepath.Clean(os.TempDir())+string(filepath.Separator))
}
