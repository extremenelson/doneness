package main

// Characterization tests (eng review D11): they pin what doneness does today,
// before main.go is restructured into `doneness new` + internal/scaffold.
// When the restructure lands, only assertions for the approved intentional
// changes may be edited (derived module prefix, verify-only `check` + `fix`,
// Stop hook via gate.HookCommand, .doneness.json, stop-check.sh removed).
// T3 applied those changes; each edited assertion is marked "T3:".

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/doneness/internal/gate"
	"github.com/extremenelson/doneness/internal/scaffold"
)

// wantFiles is every file a scaffold of "demo" contains, relative to the
// working directory, excluding git's own metadata (.git, .bare).
var wantFiles = []string{
	".claude/settings.json",
	".doneness.json", // T3: gate config written by gate.WriteConfig
	".editorconfig",
	".env.example",
	".gitattributes",
	".github/pull_request_template.md",
	".github/workflows/ci.yml",
	".gitignore",
	".golangci.yml",
	"CLAUDE.md",
	"Makefile",
	"README.md",
	"cmd/demo/main.go",
	"docs/adr/0000-template.md",
	"docs/architecture.md",
	"go.mod",
	"internal/greet/greet.go",
	"internal/greet/greet_test.go",
}

// isolateGit makes git ignore the developer's global and system config
// (commit signing, hooks, default branch) and supplies a committer identity.
func isolateGit(t *testing.T) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	// T3: the module prefix now comes from github.user; this value keeps
	// the pre-T3 default module paths.
	body := "[user]\n\tname = doneness test\n\temail = test@example.invalid\n" +
		"[github]\n\tuser = extremenelson\n" +
		"[commit]\n\tgpgsign = false\n[tag]\n\tgpgsign = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("DONENESS_PREFIX", "")
}

// scaffold runs doneness with flags, creating "demo" in a fresh parent dir,
// and returns the project directory.
func scaffoldDemo(t *testing.T, flags ...string) string {
	t.Helper()
	isolateGit(t)
	parent := t.TempDir()
	args := append(append([]string{"new"}, flags...), "-dir", parent, "demo") // T3: `new` subcommand
	if err := run(args); err != nil {
		t.Fatalf("run(%q): %v", args, err)
	}
	return filepath.Join(parent, "demo")
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// listFiles returns the regular files under root, skipping git metadata.
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var got []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name := d.Name(); path != root && (name == ".git" || name == ".bare") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(root, path)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	return got
}

func assertFiles(t *testing.T, root string) {
	t.Helper()
	if got := listFiles(t, root); !slices.Equal(got, wantFiles) {
		t.Errorf("rendered files in %s:\n got  %q\n want %q", root, got, wantFiles)
	}
}

func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseArgs(t *testing.T) {
	isolateGit(t) // T3: prefix default reads git config
	parent := t.TempDir()
	tests := []struct {
		name    string
		env     string // DONENESS_PREFIX
		args    []string
		want    scaffold.Config
		wantErr string
	}{
		{name: "defaults", args: []string{"-dir", parent, "demo"},
			want: scaffold.Config{Name: "demo", Module: "github.com/extremenelson/demo", Dir: filepath.Join(parent, "demo"), Git: true}},
		{name: "prefix flag, trailing slash trimmed", args: []string{"-prefix", "example.com/acme/", "-dir", parent, "demo"},
			want: scaffold.Config{Name: "demo", Module: "example.com/acme/demo", Dir: filepath.Join(parent, "demo"), Git: true}},
		{name: "prefix from env", env: "example.org/env", args: []string{"-dir", parent, "demo"},
			want: scaffold.Config{Name: "demo", Module: "example.org/env/demo", Dir: filepath.Join(parent, "demo"), Git: true}},
		{name: "flag beats env", env: "example.org/env", args: []string{"-prefix", "example.com/flag", "-dir", parent, "demo"},
			want: scaffold.Config{Name: "demo", Module: "example.com/flag/demo", Dir: filepath.Join(parent, "demo"), Git: true}},
		{name: "flat", args: []string{"-flat", "-dir", parent, "demo"},
			want: scaffold.Config{Name: "demo", Module: "github.com/extremenelson/demo", Dir: filepath.Join(parent, "demo"), Flat: true, Git: true}},
		{name: "no-git implies flat", args: []string{"-no-git", "-dir", parent, "demo"},
			want: scaffold.Config{Name: "demo", Module: "github.com/extremenelson/demo", Dir: filepath.Join(parent, "demo"), Flat: true}},
		{name: "beads", args: []string{"-bd", "-dir", parent, "demo"},
			want: scaffold.Config{Name: "demo", Module: "github.com/extremenelson/demo", Dir: filepath.Join(parent, "demo"), Git: true, Beads: true}},
		{name: "digits and hyphens allowed", args: []string{"-dir", parent, "cost-scan2"},
			want: scaffold.Config{Name: "cost-scan2", Module: "github.com/extremenelson/cost-scan2", Dir: filepath.Join(parent, "cost-scan2"), Git: true}},
		{name: "missing name", args: []string{"-dir", parent}, wantErr: "exactly one project name"},
		{name: "flags after name", args: []string{"demo", "-flat"}, wantErr: "flags go before the name"},
		{name: "uppercase rejected", args: []string{"Demo"}, wantErr: `invalid name "Demo"`},
		{name: "leading digit rejected", args: []string{"1demo"}, wantErr: `invalid name "1demo"`},
		{name: "underscore rejected", args: []string{"de_mo"}, wantErr: `invalid name "de_mo"`},
		{name: "unknown flag", args: []string{"-nope", "demo"}, wantErr: "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DONENESS_PREFIX", tt.env)
			got, err := parseArgsQuiet(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("config =\n %+v\nwant\n %+v", got, tt.want)
			}
		})
	}
}

// parseArgsQuiet runs parseArgs with usage output discarded.
func parseArgsQuiet(args []string) (scaffold.Config, error) {
	stderr := os.Stderr
	devnull, _ := os.Open(os.DevNull)
	os.Stderr = devnull
	defer func() { os.Stderr = stderr; _ = devnull.Close() }()
	return parseArgs(args)
}

func TestHelpIsNotAnError(t *testing.T) {
	stderr := os.Stderr
	devnull, _ := os.Open(os.DevNull)
	os.Stderr = devnull
	defer func() { os.Stderr = stderr; _ = devnull.Close() }()
	if _, err := parseArgs([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseArgs(-h) err = %v, want flag.ErrHelp", err)
	}
	if err := run([]string{"-h"}); err != nil {
		t.Fatalf("run(-h) = %v, want nil", err)
	}
}

func TestRunContainerLayout(t *testing.T) {
	dir := scaffoldDemo(t, "-prefix", "example.com/acme")
	work := filepath.Join(dir, "main")

	if fi, err := os.Stat(filepath.Join(dir, ".bare")); err != nil || !fi.IsDir() {
		t.Fatalf(".bare is not a directory: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, ".git")); got != "gitdir: ./.bare\n" {
		t.Errorf(".git pointer = %q, want %q", got, "gitdir: ./.bare\n")
	}
	assertFiles(t, work)
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Errorf("container holds %d entries, want .bare, .git, main", len(entries))
	}

	// T3: no .sh template remains; the .sh → 0755 rule is tested in internal/scaffold.
	assertMode(t, filepath.Join(work, "Makefile"), 0o644)

	if got := gitOut(t, work, "log", "--format=%s"); got != "chore: scaffold demo" {
		t.Errorf("commit log = %q, want exactly one commit %q", got, "chore: scaffold demo")
	}
	if got := gitOut(t, work, "branch", "--show-current"); got != "main" {
		t.Errorf("branch = %q, want main", got)
	}
	if got := gitOut(t, work, "status", "--porcelain"); got != "" {
		t.Errorf("working tree not clean after scaffold:\n%s", got)
	}
	if got := gitOut(t, dir, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("container repo is-bare = %q, want true", got)
	}

	if !strings.Contains(readFile(t, filepath.Join(work, "go.mod")), "module example.com/acme/demo\n") {
		t.Error("go.mod does not declare module example.com/acme/demo")
	}
	if !strings.HasPrefix(readFile(t, filepath.Join(work, "CLAUDE.md")), "# demo\n") {
		t.Error("CLAUDE.md template was not rendered with the project name")
	}
	if !strings.Contains(readFile(t, filepath.Join(work, "Makefile")), "BINARY  := demo\n") {
		t.Error("Makefile template was not rendered with the project name")
	}
}

func TestRunFlatLayout(t *testing.T) {
	dir := scaffoldDemo(t, "-flat")
	assertFiles(t, dir)
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf(".git is not a directory in a flat repo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".bare")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("flat repo has .bare (err=%v)", err)
	}
	if got := gitOut(t, dir, "log", "--format=%s"); got != "chore: scaffold demo" {
		t.Errorf("commit log = %q, want exactly one commit", got)
	}
	if got := gitOut(t, dir, "branch", "--show-current"); got != "main" {
		t.Errorf("branch = %q, want main", got)
	}
}

func TestRunNoGit(t *testing.T) {
	dir := scaffoldDemo(t, "-no-git")
	assertFiles(t, dir)
	if _, err := os.Stat(filepath.Join(dir, ".git")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("-no-git created .git (err=%v)", err)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "go.mod")), "module github.com/extremenelson/demo\n") {
		t.Error("go.mod missing default module path")
	}
}

func TestRunRefusesExistingDir(t *testing.T) {
	isolateGit(t)
	parent := t.TempDir()
	sentinel := filepath.Join(parent, "demo", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"new", "-dir", parent, "demo"})
	if err == nil || !strings.Contains(err.Error(), "already exists; refusing to overwrite") {
		t.Fatalf("err = %v, want refusal", err)
	}
	if got := readFile(t, sentinel); got != "mine" {
		t.Errorf("existing file changed to %q", got)
	}
	if entries, _ := os.ReadDir(filepath.Dir(sentinel)); len(entries) != 1 {
		t.Errorf("existing dir gained entries: %d", len(entries))
	}
}

// TestRunBeadsMissingIsBestEffort: -bd without bd on PATH warns and still
// produces a complete scaffold.
func TestRunBeadsMissingIsBestEffort(t *testing.T) {
	// A PATH holding only git and go, so an installed bd is invisible even
	// when it lives in the same directory (e.g. /opt/homebrew/bin).
	bin := t.TempDir()
	for _, tool := range []string{"git", "go"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not on PATH", tool)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	if _, err := exec.LookPath("bd"); err == nil {
		t.Fatal("bd still visible on the restricted PATH")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = w
	dir := scaffoldDemo(t, "-bd", "-flat")
	os.Stderr = stderr
	_ = w.Close()
	captured, _ := io.ReadAll(r)

	if !strings.Contains(string(captured), "warning: bd not found on PATH; skipping beads init") {
		t.Errorf("stderr = %q, want bd warning", captured)
	}
	assertFiles(t, dir)
	if got := gitOut(t, dir, "log", "--format=%s"); got != "chore: scaffold demo" {
		t.Errorf("commit log = %q", got)
	}
}

// TestGeneratedMakeCheck: a fresh scaffold passes its own `make check` and
// leaves the tree clean. Slow (golangci-lint, race tests): skipped with -short.
func TestGeneratedMakeCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: runs the generated project's make check")
	}
	for _, tool := range []string{"make", "golangci-lint"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	dir := scaffoldDemo(t, "-flat", "-prefix", "example.com/acme")
	cmd := exec.Command("make", "check")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make check failed: %v\n%s", err, out)
	}
	if got := gitOut(t, dir, "status", "--porcelain"); got != "" {
		t.Errorf("make check left the tree dirty:\n%s", got)
	}

	// T3: the generated project passes its own done-gate (make check via
	// .doneness.json), so a fresh scaffold starts green.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	res, err := gate.Run(context.Background(), gate.Options{Root: dir, Slots: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != gate.Pass {
		t.Errorf("generated repo's gate = %s: %s", res.Outcome, res.Detail)
	}
}

// TestCheckCommandExitCodes drives `doneness check` through run() and checks
// the exit-code contract (eng review D8): 0 pass, 1 failed, 2 config error,
// 3 tree changed, 4 timed out. Intentional change (D3): "gate" is now a
// subcommand, so it can no longer be used as a project name.
func TestCheckCommandExitCodes(t *testing.T) {
	tests := []struct {
		name    string
		command string // "" = no .doneness.json
		args    []string
		want    int
	}{
		{name: "pass", command: "true", want: 0},
		{name: "cached pass", command: "true", want: 0},
		{name: "check fails", command: "exit 7", want: 1},
		{name: "no config", want: 2},
		{name: "command not found", command: "no-such-command-xyz", want: 2},
		{name: "check writes a file", command: "echo x >> README", want: 3},
		{name: "budget exceeded", command: "sleep 5", args: []string{"--budget", "300ms"}, want: 4},
		{name: "unknown flag", command: "true", args: []string{"--nope"}, want: 2},
		{name: "stray argument", command: "true", args: []string{"extra"}, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateGit(t)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("DONENESS_SLOTS", "2")
			repo := t.TempDir()
			gitOut(t, repo, "init", "-q")
			if err := os.WriteFile(filepath.Join(repo, "README"), []byte("hi\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tt.command != "" {
				if err := gate.WriteConfig(repo, gate.Config{Gate: gate.Gate{Command: tt.command}}); err != nil {
					t.Fatal(err)
				}
			}
			gitOut(t, repo, "add", "-A")
			gitOut(t, repo, "commit", "-q", "-m", "init")
			t.Chdir(repo)
			if tt.name == "cached pass" {
				if err := run([]string{"check"}); err != nil {
					t.Fatalf("first run: %v", err)
				}
			}

			err := run(append([]string{"check"}, tt.args...))
			got := 0
			if err != nil {
				var ec exitCode
				if !errors.As(err, &ec) {
					t.Fatalf("run returned a non-exit error: %v", err)
				}
				got = int(ec)
			}
			if got != tt.want {
				t.Errorf("exit code = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCheckCommandOutsideRepo(t *testing.T) {
	isolateGit(t)
	t.Chdir(t.TempDir())
	var ec exitCode
	if err := run([]string{"check"}); !errors.As(err, &ec) || ec != 2 {
		t.Fatalf("run(check) outside a repo = %v, want exit 2", err)
	}
}

// TestHookEndToEnd runs the real binary through the exact hook command
// adopt will install (gate.HookCommand), the way Claude Code runs it: sh -c,
// Stop-hook JSON on stdin, JSON decision on stdout, exit 0.
func TestHookEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the doneness binary")
	}
	isolateGit(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bin := filepath.Join(t.TempDir(), "doneness")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	hookCmd, err := gate.HookCommand(bin, 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	repo := t.TempDir()
	repo, _ = filepath.EvalSymlinks(repo)
	gitOut(t, repo, "init", "-q")
	if err := gate.WriteConfig(repo, gate.Config{Gate: gate.Gate{Command: "test -f DONE"}}); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "add", "-A")
	gitOut(t, repo, "commit", "-q", "-m", "init")

	stop := func() map[string]any {
		t.Helper()
		in := fmt.Sprintf(`{"session_id":"e2e","cwd":%q,"stop_hook_active":false,"hook_event_name":"Stop"}`, repo)
		cmd := exec.Command("/bin/sh", "-c", hookCmd)
		cmd.Dir = t.TempDir() // Claude Code's cwd need not be the repo
		cmd.Stdin = strings.NewReader(in)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("hook exited %v (must always exit 0)", err)
		}
		var resp map[string]any
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("hook stdout is not one JSON object: %q", out)
		}
		return resp
	}

	if r := stop(); r["decision"] != "block" {
		t.Fatalf("red gate: %v, want block", r)
	}
	if r := stop(); r["decision"] != "block" {
		t.Fatalf("second red stop: %v, want block", r)
	}
	if r := stop(); r["decision"] != nil || !strings.HasPrefix(fmt.Sprint(r["systemMessage"]), "NOT DONE") {
		t.Fatalf("third red stop: %v, want allow with NOT DONE", r)
	}
	if err := os.WriteFile(filepath.Join(repo, "DONE"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := stop(); len(r) != 0 {
		t.Fatalf("green gate: %v, want {}", r)
	}
}

// T3: with no -prefix, no $DONENESS_PREFIX and no github.user, doneness new
// stops and says how to set a prefix instead of guessing one.
func TestModulePrefixRequired(t *testing.T) {
	isolateGit(t)
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	_, err := parseArgsQuiet([]string{"-dir", t.TempDir(), "demo"})
	if err == nil || !strings.Contains(err.Error(), "pass -prefix") {
		t.Fatalf("err = %v, want guidance to pass -prefix", err)
	}
}

func TestUnknownCommandAndNoArgs(t *testing.T) {
	stderr := os.Stderr
	devnull, _ := os.Open(os.DevNull)
	os.Stderr = devnull
	defer func() { os.Stderr = stderr; _ = devnull.Close() }()
	for _, args := range [][]string{nil, {"Bogus"}, {"no_such"}, {"gate"}, {"demo"}, {"-flat", "demo"}} {
		var ec exitCode
		if err := run(args); !errors.As(err, &ec) || ec != 2 {
			t.Errorf("run(%q) = %v, want exit 2 with usage", args, err)
		}
	}
}

// T3: the scaffold's Stop hook runs the gate through gate.HookCommand (PATH
// lookup, since settings.json is committed) with the 1800s timeout, and the
// gate config runs `make check`.
func TestScaffoldGateWiring(t *testing.T) {
	dir := scaffoldDemo(t, "-flat")
	var settings struct {
		Hooks struct {
			Stop []struct {
				Hooks []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
				} `json:"hooks"`
			} `json:"Stop"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, ".claude/settings.json"))), &settings); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}
	if len(settings.Hooks.Stop) != 1 || len(settings.Hooks.Stop[0].Hooks) != 1 {
		t.Fatalf("want exactly one Stop hook, got %+v", settings.Hooks.Stop)
	}
	h := settings.Hooks.Stop[0].Hooks[0]
	want, err := gate.HookCommand("", scaffold.HookTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if h.Type != "command" || h.Command != want || h.Timeout != 1800 || !gate.IsGateHook(h.Command) {
		t.Errorf("Stop hook = %+v, want command %q with timeout 1800", h, want)
	}

	cfg, err := gate.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gate.Command != "make check" {
		t.Errorf("gate command = %q, want make check", cfg.Gate.Command)
	}
	if strings.Contains(readFile(t, filepath.Join(dir, "Makefile")), "check: fmt tidy") {
		t.Error("Makefile check still depends on the rewriting fmt/tidy targets")
	}
}

// captureStdout runs f with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	f()
	os.Stdout = stdout
	_ = w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// TestStatusCommand: `doneness status [<rev>]` exits 0 when proven,
// 1 when not, 2 on error, and never runs the check.
func TestStatusCommand(t *testing.T) {
	isolateGit(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("DONENESS_SLOTS", "1")
	dir := scaffoldDemo(t, "-flat")
	t.Chdir(dir)
	code := func(err error) int {
		var ec exitCode
		if err == nil {
			return 0
		}
		if !errors.As(err, &ec) {
			t.Fatalf("non-exit error: %v", err)
		}
		return int(ec)
	}

	var err error
	out := captureStdout(t, func() { err = run([]string{"status"}) })
	if c := code(err); c != 1 || !strings.Contains(out, "working tree (tree ") || !strings.Contains(out, "  unproven") {
		t.Errorf("before gating: exit %d, output:\n%s", c, out)
	}

	if err := gate.WriteConfig(dir, gate.Config{Gate: gate.Gate{Command: "true"}}); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "commit", "-q", "-am", "fast gate for the test")
	if err := run([]string{"check"}); err != nil {
		t.Fatalf("gate: %v", err)
	}
	out = captureStdout(t, func() { err = run([]string{"status"}) })
	if c := code(err); c != 0 || !strings.Contains(out, `  PASS `) || !strings.Contains(out, `"true"`) || strings.Contains(out, "warning:") {
		t.Errorf("after a pass: exit %d, output:\n%s", c, out)
	}

	out = captureStdout(t, func() { err = run([]string{"status", "HEAD"}) })
	if c := code(err); c != 0 || !strings.HasPrefix(out, "HEAD (tree ") {
		t.Errorf("status HEAD: exit %d, output:\n%s", c, out)
	}
	if c := code(run([]string{"status", "no-such-rev"})); c != 2 {
		t.Errorf("status no-such-rev: exit %d, want 2", c)
	}
	if c := code(run([]string{"status", "--hook"})); c != 2 {
		t.Errorf("status --hook (unknown flag): exit %d, want 2", c)
	}
	if c := code(run([]string{"check", "HEAD"})); c != 2 {
		t.Errorf("check with a rev: exit %d, want 2", c)
	}
}

// TestAdoptCommand drives `doneness adopt` on a plain repository: dry run
// writes nothing, --apply writes the files and runs the gate (READY / NOT
// READY), and errors exit 2.
func TestAdoptCommand(t *testing.T) {
	setup := func(t *testing.T, check string) string {
		t.Helper()
		isolateGit(t)
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("DONENESS_SLOTS", "1")
		repo, _ := filepath.EvalSymlinks(t.TempDir())
		gitOut(t, repo, "init", "-q")
		if err := os.WriteFile(filepath.Join(repo, "Makefile"), []byte("check:\n\t"+check+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitOut(t, repo, "add", "-A")
		gitOut(t, repo, "commit", "-q", "-m", "init")
		t.Chdir(repo)
		return repo
	}
	code := func(t *testing.T, err error) int {
		t.Helper()
		var ec exitCode
		if err == nil {
			return 0
		}
		if !errors.As(err, &ec) {
			t.Fatalf("non-exit error: %v", err)
		}
		return int(ec)
	}

	t.Run("dry run writes nothing", func(t *testing.T) {
		repo := setup(t, "true")
		var err error
		out := captureStdout(t, func() { err = run([]string{"adopt"}) })
		if c := code(t, err); c != 0 {
			t.Fatalf("exit %d", c)
		}
		if !strings.Contains(out, "gate command: make check") || !strings.Contains(out, "dry run: nothing was written") {
			t.Errorf("output:\n%s", out)
		}
		if st := gitOut(t, repo, "status", "--porcelain", "--ignored"); st != "" {
			t.Errorf("dry run changed the repo:\n%s", st)
		}
	})

	t.Run("apply, gate green: READY and proven", func(t *testing.T) {
		repo := setup(t, "true")
		var err error
		out := captureStdout(t, func() { err = run([]string{"adopt", "--apply"}) })
		if c := code(t, err); c != 0 {
			t.Fatalf("exit %d:\n%s", c, out)
		}
		if !strings.Contains(out, "READY: commit .doneness.json, CLAUDE.md so new worktrees inherit the gate.") {
			t.Errorf("output:\n%s", out)
		}
		if _, err := gate.LoadConfig(repo); err != nil {
			t.Errorf("config not written: %v", err)
		}
		rep, err := gate.Status(context.Background(), gate.StatusOptions{Root: repo})
		if err != nil || !rep.Proven {
			t.Errorf("after READY, status = %+v, %v; want proven", rep, err)
		}
	})

	t.Run("apply, gate red: NOT READY, files kept", func(t *testing.T) {
		repo := setup(t, "false")
		var err error
		out := captureStdout(t, func() { err = run([]string{"adopt", "--apply"}) })
		if c := code(t, err); c != 1 {
			t.Fatalf("exit %d, want 1:\n%s", c, out)
		}
		if !strings.Contains(out, "NOT READY: gate is failed") {
			t.Errorf("output:\n%s", out)
		}
		if _, err := gate.LoadConfig(repo); err != nil {
			t.Errorf("files not kept after NOT READY: %v", err)
		}
	})

	t.Run("errors exit 2", func(t *testing.T) {
		setup(t, "true")
		devnull, _ := os.Open(os.DevNull)
		stderr := os.Stderr
		os.Stderr = devnull
		defer func() { os.Stderr = stderr; _ = devnull.Close() }()
		for _, args := range [][]string{{"adopt", "--nope"}, {"adopt", "extra"}, {"adopt", "--hook-timeout", "30s"}} {
			if c := code(t, run(args)); c != 2 {
				t.Errorf("run(%q) exit %d, want 2", args, c)
			}
		}
		t.Chdir(t.TempDir())
		if c := code(t, run([]string{"adopt"})); c != 2 {
			t.Errorf("outside a repo: exit %d, want 2", c)
		}
	})
}
