package main

// Characterization tests (eng review D11): they pin what newproj does today,
// before main.go is restructured into `newproj new` + internal/scaffold.
// When the restructure lands, only assertions for the approved intentional
// changes may be edited (derived module prefix, verify-only `check` + `fix`,
// Stop hook via gate.HookCommand, .newproj.json, stop-check.sh removed).

import (
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/extremenelson/newproj/internal/gate"
)

// wantFiles is every file a scaffold of "demo" contains, relative to the
// working directory, excluding git's own metadata (.git, .bare).
var wantFiles = []string{
	".claude/hooks/stop-check.sh",
	".claude/settings.json",
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
	body := "[user]\n\tname = newproj test\n\temail = test@example.invalid\n" +
		"[commit]\n\tgpgsign = false\n[tag]\n\tgpgsign = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("NEWPROJ_PREFIX", "")
}

// scaffold runs newproj with flags, creating "demo" in a fresh parent dir,
// and returns the project directory.
func scaffold(t *testing.T, flags ...string) string {
	t.Helper()
	isolateGit(t)
	parent := t.TempDir()
	args := append(append([]string{}, flags...), "-dir", parent, "demo")
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
	parent := t.TempDir()
	tests := []struct {
		name    string
		env     string // NEWPROJ_PREFIX
		args    []string
		want    config
		wantErr string
	}{
		{name: "defaults", args: []string{"-dir", parent, "demo"},
			want: config{Name: "demo", Module: "github.com/extremenelson/demo", Dir: filepath.Join(parent, "demo"), Git: true}},
		{name: "prefix flag, trailing slash trimmed", args: []string{"-prefix", "example.com/acme/", "-dir", parent, "demo"},
			want: config{Name: "demo", Module: "example.com/acme/demo", Dir: filepath.Join(parent, "demo"), Git: true}},
		{name: "prefix from env", env: "example.org/env", args: []string{"-dir", parent, "demo"},
			want: config{Name: "demo", Module: "example.org/env/demo", Dir: filepath.Join(parent, "demo"), Git: true}},
		{name: "flag beats env", env: "example.org/env", args: []string{"-prefix", "example.com/flag", "-dir", parent, "demo"},
			want: config{Name: "demo", Module: "example.com/flag/demo", Dir: filepath.Join(parent, "demo"), Git: true}},
		{name: "flat", args: []string{"-flat", "-dir", parent, "demo"},
			want: config{Name: "demo", Module: "github.com/extremenelson/demo", Dir: filepath.Join(parent, "demo"), Flat: true, Git: true}},
		{name: "no-git implies flat", args: []string{"-no-git", "-dir", parent, "demo"},
			want: config{Name: "demo", Module: "github.com/extremenelson/demo", Dir: filepath.Join(parent, "demo"), Flat: true}},
		{name: "beads", args: []string{"-bd", "-dir", parent, "demo"},
			want: config{Name: "demo", Module: "github.com/extremenelson/demo", Dir: filepath.Join(parent, "demo"), Git: true, Beads: true}},
		{name: "digits and hyphens allowed", args: []string{"-dir", parent, "cost-scan2"},
			want: config{Name: "cost-scan2", Module: "github.com/extremenelson/cost-scan2", Dir: filepath.Join(parent, "cost-scan2"), Git: true}},
		{name: "missing name", args: []string{"-dir", parent}, wantErr: "exactly one project name"},
		{name: "flags after name", args: []string{"demo", "-flat"}, wantErr: "flags go before the name"},
		{name: "uppercase rejected", args: []string{"Demo"}, wantErr: `invalid name "Demo"`},
		{name: "leading digit rejected", args: []string{"1demo"}, wantErr: `invalid name "1demo"`},
		{name: "underscore rejected", args: []string{"de_mo"}, wantErr: `invalid name "de_mo"`},
		{name: "unknown flag", args: []string{"-nope", "demo"}, wantErr: "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NEWPROJ_PREFIX", tt.env)
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
func parseArgsQuiet(args []string) (config, error) {
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
	dir := scaffold(t, "-prefix", "example.com/acme")
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

	assertMode(t, filepath.Join(work, ".claude/hooks/stop-check.sh"), 0o755)
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
	dir := scaffold(t, "-flat")
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
	assertMode(t, filepath.Join(dir, ".claude/hooks/stop-check.sh"), 0o755)
}

func TestRunNoGit(t *testing.T) {
	dir := scaffold(t, "-no-git")
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
	err := run([]string{"-dir", parent, "demo"})
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
	dir := scaffold(t, "-bd", "-flat")
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
	dir := scaffold(t, "-flat", "-prefix", "example.com/acme")
	cmd := exec.Command("make", "check")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make check failed: %v\n%s", err, out)
	}
	if got := gitOut(t, dir, "status", "--porcelain"); got != "" {
		t.Errorf("make check left the tree dirty:\n%s", got)
	}
}

// TestGateCommandExitCodes drives `newproj gate` through run() and checks
// the exit-code contract (eng review D8): 0 pass, 1 failed, 2 config error,
// 3 tree changed, 4 timed out. Intentional change (D3): "gate" is now a
// subcommand, so it can no longer be used as a project name.
func TestGateCommandExitCodes(t *testing.T) {
	tests := []struct {
		name    string
		command string // "" = no .newproj.json
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
			t.Setenv("NEWPROJ_SLOTS", "2")
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
				if err := run([]string{"gate"}); err != nil {
					t.Fatalf("first run: %v", err)
				}
			}

			err := run(append([]string{"gate"}, tt.args...))
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

func TestGateCommandOutsideRepo(t *testing.T) {
	isolateGit(t)
	t.Chdir(t.TempDir())
	var ec exitCode
	if err := run([]string{"gate"}); !errors.As(err, &ec) || ec != 2 {
		t.Fatalf("run(gate) outside a repo = %v, want exit 2", err)
	}
}
