package adopt

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/doneness/internal/gate"
)

func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	cfg := filepath.Join(home, "gitconfig")
	body := "[user]\n\tname = adopt test\n\temail = test@example.invalid\n[commit]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repo creates a committed repository with the given files.
func repo(t *testing.T, fs map[string]string) string {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	git(t, root, "init", "-q")
	files(t, root, fs)
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	return root
}

func fakeBin(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), gate.BinaryName)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func build(t *testing.T, o Options) *Plan {
	t.Helper()
	if o.HookTimeout == 0 {
		o.HookTimeout = 1800 * time.Second
	}
	p, err := Build(context.Background(), o)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return p
}

func change(p *Plan, rel string) *Change {
	for i := range p.Changes {
		if p.Changes[i].Rel == rel || strings.HasSuffix(p.Changes[i].Path, "/"+rel) {
			return &p.Changes[i]
		}
	}
	return nil
}

var makeRepo = map[string]string{"Makefile": "check:\n\tgo test ./...\n", "go.mod": "module x\n"}

func TestBuildDryRunWritesNothing(t *testing.T) {
	isolate(t)
	root := repo(t, makeRepo)
	before := git(t, root, "status", "--porcelain", "--ignored")
	p := build(t, Options{Root: root, Binary: fakeBin(t)})
	var out bytes.Buffer
	if err := p.Write(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if after := git(t, root, "status", "--porcelain", "--ignored"); after != before {
		t.Errorf("Build/Write touched the repo:\n%s", after)
	}
	for _, want := range []string{
		"gate command: make check\n  (from Makefile target check)",
		"other candidate: go vet ./... && go test ./...  (go.mod)",
		"+++ b/.doneness.json",
		"+++ b/.claude/settings.local.json",
		"+++ b/CLAUDE.md",
		"side effect: --apply runs `doneness check` once",
		"side effect: append /.claude/settings.local.json to ",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan output missing %q:\n%s", want, out.String())
		}
	}
}

func TestCommandPrecedence(t *testing.T) {
	isolate(t)
	root := repo(t, makeRepo)
	if p := build(t, Options{Root: root, Binary: fakeBin(t)}); p.Command != "make check" || p.CommandSource != "Makefile target check" {
		t.Errorf("detected = %q from %q", p.Command, p.CommandSource)
	}
	if p := build(t, Options{Root: root, Command: "make lint", Binary: fakeBin(t)}); p.Command != "make lint" || p.CommandSource != "--cmd" {
		t.Errorf("--cmd = %q from %q", p.Command, p.CommandSource)
	}
	if err := gate.WriteConfig(root, gate.Config{Gate: gate.Gate{Command: "make everything"}}); err != nil {
		t.Fatal(err)
	}
	if p := build(t, Options{Root: root, Binary: fakeBin(t)}); p.Command != "make everything" || !strings.Contains(p.CommandSource, "existing") {
		t.Errorf("re-run kept = %q from %q, want the existing config", p.Command, p.CommandSource)
	}
}

func TestBuildErrors(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	empty := repo(t, map[string]string{"README.md": "hi\n"})
	if _, err := Build(ctx, Options{Root: empty, HookTimeout: time.Hour, Binary: fakeBin(t)}); err == nil || !strings.Contains(err.Error(), "pass --cmd") {
		t.Errorf("no candidates: err = %v", err)
	}
	if _, err := Build(ctx, Options{Root: empty, Command: "true", HookTimeout: 59 * time.Second, Binary: fakeBin(t)}); err == nil || !strings.Contains(err.Error(), "minimum") {
		t.Errorf("short timeout: err = %v", err)
	}
	bad := repo(t, map[string]string{gate.ConfigFile: `{"gate":{"comand":"x"}}`})
	if _, err := Build(ctx, Options{Root: bad, HookTimeout: time.Hour, Binary: fakeBin(t)}); err == nil || !strings.Contains(err.Error(), "fix it or pass --cmd") {
		t.Errorf("invalid existing config: err = %v", err)
	}
	if _, err := Build(ctx, Options{Root: empty, Command: "true", HookTimeout: time.Hour, Binary: "relative/doneness"}); err == nil {
		t.Error("local hook accepted a relative binary path")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	isolate(t)
	root := repo(t, map[string]string{"Makefile": "check:\n\ttrue\n", "CLAUDE.md": "# Project\n\nExisting rules.\n"})
	bin := fakeBin(t)
	p := build(t, Options{Root: root, Binary: bin})
	if err := p.Apply(); err != nil {
		t.Fatal(err)
	}

	cfg, err := gate.LoadConfig(root)
	if err != nil || cfg.Gate.Command != "make check" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	claude, _ := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if !strings.HasPrefix(string(claude), "# Project\n\nExisting rules.\n\n"+blockBegin) {
		t.Errorf("rules block not appended after existing content:\n%s", claude)
	}
	settings, _ := os.ReadFile(filepath.Join(root, ".claude", "settings.local.json"))
	cmds := stopCommands(t, settings)
	if len(cmds) != 1 || !gate.IsGateHook(cmds[0]) {
		t.Fatalf("hook commands = %q", cmds)
	}
	if b, _ := gate.HookBinary(cmds[0]); b != bin {
		t.Errorf("local hook calls %q, want the absolute binary %q", b, bin)
	}
	if out := git(t, root, "status", "--porcelain"); strings.Contains(out, "settings.local.json") {
		t.Errorf("local settings not ignored after apply:\n%s", out)
	}

	again := build(t, Options{Root: root, Binary: bin})
	if len(again.Changes) != 0 {
		var names []string
		for _, c := range again.Changes {
			names = append(names, c.Rel)
		}
		t.Errorf("second adopt still changes %v", names)
	}
	for _, s := range again.SideEffects {
		if strings.Contains(s, "info/exclude") {
			t.Error("second adopt would append the exclude entry again")
		}
	}
}

func TestRulesBlockReplacedNotDuplicated(t *testing.T) {
	old := []byte("intro\n\n" + rulesBlock("make old") + "\noutro\n")
	got := string(withRulesBlock(old, rulesBlock("make new")))
	if strings.Count(got, blockBegin) != 1 || !strings.Contains(got, "`make new`") || strings.Contains(got, "`make old`") {
		t.Errorf("block not replaced in place:\n%s", got)
	}
	if !strings.HasPrefix(got, "intro\n\n") || !strings.HasSuffix(got, "\noutro\n") {
		t.Errorf("surrounding text changed:\n%s", got)
	}
}

func TestRulesFileChoice(t *testing.T) {
	isolate(t)
	tests := []struct {
		name  string
		files map[string]string
		flag  string
		want  string
	}{
		{"neither: new CLAUDE.md", nil, "", "CLAUDE.md"},
		{"only AGENTS.md", map[string]string{"AGENTS.md": "rules\n"}, "", "AGENTS.md"},
		{"both: CLAUDE.md", map[string]string{"AGENTS.md": "a\n", "CLAUDE.md": "c\n"}, "", "CLAUDE.md"},
		{"flag wins", map[string]string{"CLAUDE.md": "c\n"}, "AGENTS.md", "AGENTS.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := map[string]string{"Makefile": "check:\n\ttrue\n"}
			for k, v := range tt.files {
				fs[k] = v
			}
			root := repo(t, fs)
			p := build(t, Options{Root: root, AgentsFile: tt.flag, Binary: fakeBin(t)})
			if c := change(p, tt.want); c == nil || !strings.Contains(string(c.New), blockBegin) {
				t.Errorf("rules block not planned for %s", tt.want)
			}
		})
	}
}

func TestSharedHook(t *testing.T) {
	isolate(t)
	root := repo(t, map[string]string{
		"Makefile":              "check:\n\ttrue\n",
		".claude/settings.json": "{\n  \"model\": \"opus\",\n  \"hooks\": {\n    \"Stop\": []\n  }\n}\n",
	})
	p := build(t, Options{Root: root, Shared: true})
	c := change(p, ".claude/settings.json")
	if c == nil {
		t.Fatal("no settings.json change")
	}
	if !strings.HasPrefix(string(c.New), "{\n  \"model\": \"opus\",\n  \"hooks\": {\n    \"Stop\": [\n") {
		t.Errorf("shared settings were reformatted:\n%s", c.New)
	}
	cmds := stopCommands(t, c.New)
	if b, ok := gate.HookBinary(cmds[0]); !ok || b != "" {
		t.Errorf("shared hook = %q, want a PATH lookup", cmds[0])
	}
	for _, s := range p.SideEffects {
		if strings.Contains(s, "info/exclude") {
			t.Error("shared mode would edit info/exclude")
		}
	}
}

func TestLocalHookGoesToMainCheckout(t *testing.T) {
	isolate(t)
	root := repo(t, map[string]string{"Makefile": "check:\n\ttrue\n"})
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, root, "worktree", "add", "-q", "--detach", wt)
	wt, _ = filepath.EvalSymlinks(wt)
	p := build(t, Options{Root: wt, Binary: fakeBin(t)})
	if want := filepath.Join(root, ".claude", "settings.local.json"); p.HookFile != want {
		t.Errorf("hook file = %s, want the main checkout's %s", p.HookFile, want)
	}
	if c := change(p, gate.ConfigFile); c == nil || c.Path != filepath.Join(wt, gate.ConfigFile) {
		t.Error("gate config should be written in the worktree being adopted")
	}
}

func TestTemporaryBinaryWarning(t *testing.T) {
	isolate(t)
	root := repo(t, map[string]string{"Makefile": "check:\n\ttrue\n"})
	p := build(t, Options{Root: root, Binary: filepath.Join(os.TempDir(), "go-build123", "b001", "exe", "doneness")})
	if len(p.Warnings) == 0 || !strings.Contains(p.Warnings[0], "temporary build") {
		t.Errorf("warnings = %q", p.Warnings)
	}
}

func TestApplyTwiceAddsExcludeOnce(t *testing.T) {
	isolate(t)
	root := repo(t, map[string]string{"Makefile": "check:\n\ttrue\n"})
	p := build(t, Options{Root: root, Binary: fakeBin(t)})
	if p.exclude == nil {
		t.Fatal("plan has no exclude edit for an un-ignored local settings file")
	}
	for range 2 {
		if err := p.Apply(); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(p.exclude.path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), p.exclude.line+"\n"); n != 1 {
		t.Errorf("exclude line appears %d times:\n%s", n, b)
	}
}
