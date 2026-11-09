package gate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// installHook writes a Stop hook for the gate into dir/.claude/<file>.
func installHook(t *testing.T, dir, file, bin string) {
	t.Helper()
	cmd, err := HookCommand(bin, 600*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{"hooks": map[string]any{"Stop": []any{
		map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd, "timeout": 600}}},
	}}}
	b, _ := json.Marshal(settings)
	write(t, filepath.Join(dir, ".claude", file), string(b))
}

// fakeBinary creates an executable file to stand in for the gate binary.
func fakeBinary(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), BinaryName)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// readyRepo is a committed repo with a committed shared hook, so a fresh
// status has no integrity problems.
func readyRepo(t *testing.T, command string) string {
	t.Helper()
	root := newRepo(t, command)
	installHook(t, root, "settings.json", "")
	run(t, root, "git", "add", "-A")
	run(t, root, "git", "commit", "-q", "-m", "hook")
	return root
}

func status(t *testing.T, o StatusOptions) StatusReport {
	t.Helper()
	if o.Version == "" {
		o.Version = "test"
	}
	rep, err := Status(context.Background(), o)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return rep
}

func hasProblem(rep StatusReport, prefix string) bool {
	for _, p := range rep.Problems {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func TestStatusProvenAfterPass(t *testing.T) {
	isolate(t)
	root := readyRepo(t, "true")
	state := t.TempDir()
	o := opts(root)
	o.StateDir = state
	if res := mustRun(t, context.Background(), o); res.Outcome != Pass {
		t.Fatalf("run = %s", res.Outcome)
	}
	rep := status(t, StatusOptions{Root: root, StateDir: state})
	if !rep.Proven || rep.Receipt == nil || rep.Stale != "" {
		t.Fatalf("status = %+v, want proven", rep)
	}
	if rep.Target != "working tree" || rep.Command != "true" {
		t.Errorf("target/command = %q/%q", rep.Target, rep.Command)
	}
	if len(rep.Problems) != 0 {
		t.Errorf("problems on a clean, hooked, committed repo: %q", rep.Problems)
	}
}

func TestStatusNeverRunsTheCheck(t *testing.T) {
	isolate(t)
	marker := filepath.Join(t.TempDir(), "ran")
	root := readyRepo(t, "touch "+marker)
	_ = status(t, StatusOptions{Root: root, StateDir: t.TempDir()})
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("Status ran the check")
	}
}

func TestStatusUnprovenAndLastRed(t *testing.T) {
	isolate(t)
	root := readyRepo(t, "exit 1")
	state := t.TempDir()
	if rep := status(t, StatusOptions{Root: root, StateDir: state}); rep.Proven || rep.LastRed != nil {
		t.Fatalf("before any run: %+v, want unproven with no red", rep)
	}
	o := opts(root)
	o.StateDir = state
	_ = mustRun(t, context.Background(), o)
	rep := status(t, StatusOptions{Root: root, StateDir: state})
	if rep.Proven || rep.LastRed == nil || rep.LastRed.Kind != "failed" {
		t.Fatalf("after a failed run: %+v, want unproven, last red failed", rep)
	}
}

func TestStatusStaleReceipts(t *testing.T) {
	isolate(t)
	root := readyRepo(t, "true")
	tree := hash(t, root)
	cfg, _ := LoadConfig(root)
	base := Receipt{Tree: tree, Command: cfg.Gate.Command, ConfigHash: cfg.Hash(), At: time.Now().UTC(),
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Version: "test"}
	tests := []struct {
		name   string
		mutate func(*Receipt)
		want   string
	}{
		{"config", func(r *Receipt) { r.Command, r.ConfigHash = "make other", "x" }, "config drift"},
		{"version", func(r *Receipt) { r.Version = "v9" }, "made by doneness v9, this is test"},
		{"platform", func(r *Receipt) { r.GOOS = "plan9" }, "made on plan9/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := base
			tt.mutate(&r)
			if err := writeReceipt(context.Background(), root, r); err != nil {
				t.Fatal(err)
			}
			rep := status(t, StatusOptions{Root: root, StateDir: t.TempDir()})
			if rep.Proven || !strings.Contains(rep.Stale, tt.want) {
				t.Errorf("stale = %q (proven %v), want containing %q", rep.Stale, rep.Proven, tt.want)
			}
		})
	}
}

func TestStatusUncommittedConfig(t *testing.T) {
	isolate(t)
	root := readyRepo(t, "true")
	if rep := status(t, StatusOptions{Root: root, StateDir: t.TempDir()}); hasProblem(rep, "uncommitted config") {
		t.Fatal("committed config reported as uncommitted")
	}
	if err := WriteConfig(root, Config{Gate: Gate{Command: "make check"}}); err != nil {
		t.Fatal(err)
	}
	if rep := status(t, StatusOptions{Root: root, StateDir: t.TempDir()}); !hasProblem(rep, "uncommitted config") {
		t.Errorf("edited config not reported: %q", rep.Problems)
	}
}

func TestStatusConfigDrift(t *testing.T) {
	isolate(t)
	root := readyRepo(t, "true")
	if rep := status(t, StatusOptions{Root: root, StateDir: t.TempDir()}); hasProblem(rep, "config drift") {
		t.Fatal("drift reported on the default branch itself")
	}
	run(t, root, "git", "switch", "-q", "-c", "feature")
	if rep := status(t, StatusOptions{Root: root, StateDir: t.TempDir()}); hasProblem(rep, "config drift") {
		t.Fatal("drift reported on a branch that did not touch the config")
	}
	if err := WriteConfig(root, Config{Gate: Gate{Command: "true # weakened"}}); err != nil {
		t.Fatal(err)
	}
	run(t, root, "git", "commit", "-q", "-am", "weaken the gate")
	rep := status(t, StatusOptions{Root: root, StateDir: t.TempDir()})
	if !hasProblem(rep, "config drift: "+ConfigFile+" changed since main") {
		t.Errorf("branch that changed the config: problems %q", rep.Problems)
	}
}

func TestStatusRev(t *testing.T) {
	isolate(t)
	root := readyRepo(t, "true")
	state := t.TempDir()
	o := opts(root)
	o.StateDir = state
	if res := mustRun(t, context.Background(), o); res.Outcome != Pass {
		t.Fatal(res.Outcome)
	}
	write(t, filepath.Join(root, "tracked.txt"), "v2\n")
	// The newer commit also changes the gate config: --status <rev> must
	// judge HEAD~1 by the config stored in HEAD~1, not the current one.
	if err := WriteConfig(root, Config{Gate: Gate{Command: "true # stricter"}}); err != nil {
		t.Fatal(err)
	}
	run(t, root, "git", "commit", "-q", "-am", "unproven change")

	if rep := status(t, StatusOptions{Root: root, Rev: "HEAD~1", StateDir: state}); !rep.Proven || rep.Target != "HEAD~1" {
		t.Errorf("HEAD~1 (gated) = %+v, want proven", rep)
	}
	if rep := status(t, StatusOptions{Root: root, Rev: "HEAD", StateDir: state}); rep.Proven {
		t.Error("HEAD (never gated) reported proven")
	}
	if _, err := Status(context.Background(), StatusOptions{Root: root, Rev: "no-such-rev", StateDir: state, Version: "test"}); err == nil {
		t.Error("unknown rev accepted")
	}
}

func TestStatusHook(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		isolate(t)
		root := newRepo(t, "true")
		if rep := status(t, StatusOptions{Root: root, StateDir: t.TempDir()}); !hasProblem(rep, "hook missing") {
			t.Errorf("no hook installed: problems %q", rep.Problems)
		}
	})
	t.Run("local hook with a live binary", func(t *testing.T) {
		isolate(t)
		root := newRepo(t, "true")
		installHook(t, root, "settings.local.json", fakeBinary(t))
		if rep := status(t, StatusOptions{Root: root, StateDir: t.TempDir()}); hasProblem(rep, "hook") {
			t.Errorf("healthy local hook: problems %q", rep.Problems)
		}
	})
	t.Run("local hook whose binary is gone", func(t *testing.T) {
		isolate(t)
		root := newRepo(t, "true")
		bin := fakeBinary(t)
		installHook(t, root, "settings.local.json", bin)
		if err := os.Remove(bin); err != nil {
			t.Fatal(err)
		}
		if rep := status(t, StatusOptions{Root: root, StateDir: t.TempDir()}); !hasProblem(rep, "hook binary missing: "+bin) {
			t.Errorf("deleted binary: problems %q", rep.Problems)
		}
	})
	t.Run("worktree uses the main checkout's local hook", func(t *testing.T) {
		isolate(t)
		root := newRepo(t, "true")
		installHook(t, root, "settings.local.json", fakeBinary(t))
		wt := filepath.Join(t.TempDir(), "wt")
		run(t, root, "git", "worktree", "add", "-q", "--detach", wt)
		wt, _ = filepath.EvalSymlinks(wt)
		if rep := status(t, StatusOptions{Root: wt, StateDir: t.TempDir()}); hasProblem(rep, "hook") {
			t.Errorf("worktree of a hooked main checkout: problems %q", rep.Problems)
		}
	})
	t.Run("unreadable settings", func(t *testing.T) {
		isolate(t)
		root := newRepo(t, "true")
		write(t, filepath.Join(root, ".claude", "settings.json"), "{not json")
		rep := status(t, StatusOptions{Root: root, StateDir: t.TempDir()})
		if !hasProblem(rep, "hook settings unreadable") || !hasProblem(rep, "hook missing") {
			t.Errorf("malformed settings: problems %q", rep.Problems)
		}
	})
	t.Run("other hooks do not count", func(t *testing.T) {
		isolate(t)
		root := newRepo(t, "true")
		write(t, filepath.Join(root, ".claude", "settings.json"),
			`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"make check"}]}]}}`)
		if rep := status(t, StatusOptions{Root: root, StateDir: t.TempDir()}); !hasProblem(rep, "hook missing") {
			t.Errorf("a non-gate Stop hook satisfied the check: %q", rep.Problems)
		}
	})
}

func TestDefaultBranch(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	root := newRepo(t, "true")
	if b, ok := DefaultBranch(ctx, root); !ok || b != "main" {
		t.Errorf("plain repo = %q, %v; want main", b, ok)
	}

	run(t, root, "git", "branch", "-m", "main", "trunk")
	if _, ok := DefaultBranch(ctx, root); ok {
		t.Error("found a default branch with no main, no init.defaultBranch and no origin")
	}
	run(t, root, "git", "config", "init.defaultBranch", "trunk")
	if b, ok := DefaultBranch(ctx, root); !ok || b != "trunk" {
		t.Errorf("init.defaultBranch = %q, %v; want trunk", b, ok)
	}

	run(t, root, "git", "update-ref", "refs/remotes/origin/dev", "HEAD")
	run(t, root, "git", "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/dev")
	if b, ok := DefaultBranch(ctx, root); !ok || b != "origin/dev" {
		t.Errorf("origin/HEAD = %q, %v; want origin/dev", b, ok)
	}
}

func TestHookBinary(t *testing.T) {
	for _, bin := range []string{"", "/usr/local/bin/doneness", "/opt/it's here/doneness"} {
		cmd, err := HookCommand(bin, 600*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := HookBinary(cmd)
		if !ok || got != bin {
			t.Errorf("HookBinary(HookCommand(%q)) = %q, %v", bin, got, ok)
		}
	}
	tests := []struct {
		cmd, want string
		ok        bool
	}{
		{"doneness hook", "", true},
		{"/x/y/doneness hook --budget 30s", "/x/y/doneness", true},
		{"make check", "", false},
		{"echo doneness hook", "", false},
	}
	for _, tt := range tests {
		if got, ok := HookBinary(tt.cmd); got != tt.want || ok != tt.ok {
			t.Errorf("HookBinary(%q) = %q, %v; want %q, %v", tt.cmd, got, ok, tt.want, tt.ok)
		}
	}
}
