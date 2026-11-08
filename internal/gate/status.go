package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// StatusOptions configures Status.
type StatusOptions struct {
	Root     string // work tree top level
	Rev      string // "" for the working tree, else a commit-ish
	StateDir string // "": the default state dir
	Version  string // "": Version()
}

// StatusReport answers "is this tree proven, and can I trust the setup
// that proved it?" without running the check.
type StatusReport struct {
	Tree     string
	Target   string   // "working tree" or the rev
	Command  string   // the gate command for this tree ("" if no config)
	Receipt  *Receipt // the PASS receipt for Tree, if any
	Proven   bool     // Receipt satisfies this tree's config, version and platform
	Stale    string   // why an existing receipt does not count
	LastRed  *LastRed // latest local failure for Tree, when not proven
	Problems []string // integrity warnings: config, hook
}

// Status reports whether opts.Rev (or the working tree) has a receipt that
// proves it, plus integrity problems: an uncommitted or drifted gate config,
// and a missing or broken Stop hook. It never runs the check.
func Status(ctx context.Context, opts StatusOptions) (StatusReport, error) {
	version := opts.Version
	if version == "" {
		version = Version()
	}
	state := opts.StateDir
	if state == "" {
		var err error
		if state, err = stateDir(); err != nil {
			return StatusReport{}, err
		}
	}

	rep := StatusReport{Target: "working tree"}
	var cfg Config
	var cfgErr error
	if opts.Rev == "" {
		tree, err := TreeHash(ctx, opts.Root)
		if err != nil {
			return rep, err
		}
		rep.Tree = tree
		cfg, cfgErr = LoadConfig(opts.Root)
	} else {
		rep.Target = opts.Rev
		tree, err := git(ctx, opts.Root, nil, nil, "rev-parse", "--verify", "--quiet", opts.Rev+"^{tree}")
		if err != nil {
			return rep, fmt.Errorf("%s does not name a commit or tree", opts.Rev)
		}
		rep.Tree = tree
		cfg, cfgErr = configAt(ctx, opts.Root, opts.Rev)
	}
	if cfgErr != nil {
		rep.Problems = append(rep.Problems, "gate config: "+cfgErr.Error())
	} else {
		rep.Command = cfg.Gate.Command
	}

	r, err := ReadReceipt(ctx, opts.Root, rep.Tree)
	if err != nil {
		return rep, err
	}
	rep.Receipt = r
	switch {
	case r == nil:
	case cfgErr != nil:
		rep.Stale = "this tree has no valid gate config"
	case r.Command != cfg.Gate.Command || r.ConfigHash != cfg.Hash():
		rep.Stale = "config drift: the receipt was made with a different gate config"
	case r.Version != version:
		rep.Stale = fmt.Sprintf("made by newproj %s, this is %s", r.Version, version)
	case !r.satisfies(cfg, version):
		rep.Stale = fmt.Sprintf("made on %s/%s", r.GOOS, r.GOARCH)
	default:
		rep.Proven = true
	}
	if !rep.Proven {
		if rep.LastRed, err = ReadLastRed(state, rep.Tree); err != nil {
			return rep, err
		}
	}

	if opts.Rev == "" && cfgErr == nil {
		if p, err := uncommittedConfig(ctx, opts.Root); err != nil {
			return rep, err
		} else if p != "" {
			rep.Problems = append(rep.Problems, p)
		}
	}
	if p, err := configDrift(ctx, opts.Root, opts.Rev); err != nil {
		return rep, err
	} else if p != "" {
		rep.Problems = append(rep.Problems, p)
	}
	hookProblems, err := hookStatus(ctx, opts.Root)
	if err != nil {
		return rep, err
	}
	rep.Problems = append(rep.Problems, hookProblems...)
	return rep, nil
}

// configAt parses ConfigFile as stored in rev.
func configAt(ctx context.Context, root, rev string) (Config, error) {
	out, err := git(ctx, root, nil, nil, "show", rev+":"+ConfigFile)
	if err != nil {
		return Config{}, ErrNoConfig
	}
	return ParseConfig([]byte(out))
}

// uncommittedConfig reports a ConfigFile that differs from HEAD (or is
// untracked): worktrees created from the branch would not get it.
func uncommittedConfig(ctx context.Context, root string) (string, error) {
	out, err := git(ctx, root, nil, nil, "status", "--porcelain", "--", ConfigFile)
	if err != nil {
		return "", err
	}
	if out != "" {
		return "uncommitted config: commit " + ConfigFile + " so new worktrees inherit the gate", nil
	}
	return "", nil
}

// DefaultBranch resolves the repository's default branch: origin/HEAD,
// else init.defaultBranch, else main. ok is false when none exists.
func DefaultBranch(ctx context.Context, root string) (ref string, ok bool) {
	if out, err := git(ctx, root, nil, nil, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil && out != "" {
		return out, true
	}
	candidates := []string{}
	if out, err := git(ctx, root, nil, nil, "config", "--get", "init.defaultBranch"); err == nil && out != "" {
		candidates = append(candidates, out)
	}
	candidates = append(candidates, "main")
	for _, b := range candidates {
		if _, err := git(ctx, root, nil, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+b); err == nil {
			return b, true
		}
	}
	return "", false
}

// configDrift reports a ConfigFile at rev (HEAD for the working tree) that
// differs from the merge base with the default branch: someone changed what
// "done" means on this branch.
func configDrift(ctx context.Context, root, rev string) (string, error) {
	if rev == "" {
		rev = "HEAD"
	}
	if _, err := git(ctx, root, nil, nil, "rev-parse", "--verify", "--quiet", rev+"^{commit}"); err != nil {
		return "", nil // no commits yet, or a bare tree: nothing to compare
	}
	base, ok := DefaultBranch(ctx, root)
	if !ok {
		return "", nil
	}
	mb, err := git(ctx, root, nil, nil, "merge-base", rev, base)
	if err != nil {
		return "", nil // unrelated histories
	}
	_, err = git(ctx, root, nil, nil, "diff", "--quiet", mb, rev, "--", ConfigFile)
	if err == nil {
		return "", nil
	}
	var ge *gitError
	if errors.As(err, &ge) && strings.TrimSpace(ge.stderr) == "" {
		return fmt.Sprintf("config drift: %s changed since %s (merge base %s)", ConfigFile, base, short(mb)), nil
	}
	return "", err
}

// hookStatus checks that a Claude Code Stop hook runs the gate, in the
// settings files Claude Code reads for this repository, and that a hook
// pinned to an absolute binary path still points at an executable.
func hookStatus(ctx context.Context, root string) ([]string, error) {
	files := []string{
		filepath.Join(root, ".claude", "settings.json"),
		filepath.Join(root, ".claude", "settings.local.json"),
	}
	if main, err := mainCheckout(ctx, root); err == nil && main != root {
		// Claude Code reads a worktree's local settings from the main checkout.
		files = append(files, filepath.Join(main, ".claude", "settings.local.json"))
	}

	var problems []string
	found := false
	for _, f := range files {
		cmds, err := stopHookCommands(f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("hook settings unreadable: %s: %v", f, err))
			continue
		}
		for _, c := range cmds {
			bin, ok := HookBinary(c)
			if !ok {
				continue
			}
			found = true
			if filepath.IsAbs(bin) {
				if fi, err := os.Stat(bin); err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
					problems = append(problems, fmt.Sprintf("hook binary missing: %s (in %s); re-run `newproj adopt --apply`", bin, f))
				}
			}
		}
	}
	if !found {
		problems = append(problems, "hook missing: no `newproj gate --hook` Stop hook in .claude/settings.json or settings.local.json; run `newproj adopt --apply`")
	}
	return problems, nil
}

// mainCheckout is the repository's main working tree (the first entry of
// `git worktree list`), or root when the repository is bare.
func mainCheckout(ctx context.Context, root string) (string, error) {
	out, err := git(ctx, root, nil, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	lines := strings.Split(out, "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "worktree ") {
		return root, nil
	}
	if len(lines) > 1 && lines[1] == "bare" {
		return root, nil
	}
	return strings.TrimPrefix(lines[0], "worktree "), nil
}

// stopHookCommands returns the command strings of every Stop hook in a
// Claude Code settings file.
func stopHookCommands(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s struct {
		Hooks struct {
			Stop []struct {
				Hooks []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"Stop"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	var cmds []string
	for _, group := range s.Hooks.Stop {
		for _, h := range group.Hooks {
			if h.Type == "command" {
				cmds = append(cmds, h.Command)
			}
		}
	}
	return cmds, nil
}
