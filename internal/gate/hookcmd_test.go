package gate

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeGate writes an executable named BinaryName into dir that records its
// arguments to dir/args and prints a marker on stdout.
func fakeGate(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, BinaryName)
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$(dirname \"$0\")/args\"\necho FAKE_GATE_RAN\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// runHook executes a hook command the way Claude Code does (via sh) with
// the given PATH, returning stdout and the exit error.
func runHook(t *testing.T, cmd, path string) (string, error) {
	t.Helper()
	c := exec.Command("/bin/sh", "-c", cmd)
	c.Env = []string{"PATH=" + path}
	out, err := c.Output()
	return string(out), err
}

func TestHookCommandRunsGate(t *testing.T) {
	sysPath := "/usr/bin:/bin"
	tests := []struct {
		name    string
		timeout time.Duration
		// setup returns (bin argument, PATH for the hook, dir holding args)
		setup func(t *testing.T) (string, string, string)
	}{
		{name: "absolute path", timeout: 1800 * time.Second, setup: func(t *testing.T) (string, string, string) {
			dir := t.TempDir()
			return fakeGate(t, dir), sysPath, dir
		}},
		{name: "absolute path with spaces and quotes", timeout: 600 * time.Second, setup: func(t *testing.T) (string, string, string) {
			dir := filepath.Join(t.TempDir(), "it's a dir")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return fakeGate(t, dir), sysPath, dir
		}},
		{name: "PATH lookup", timeout: MinHookTimeout, setup: func(t *testing.T) (string, string, string) {
			dir := t.TempDir()
			fakeGate(t, dir)
			return "", dir + ":" + sysPath, dir
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, path, dir := tt.setup(t)
			cmd, err := HookCommand(bin, tt.timeout)
			if err != nil {
				t.Fatal(err)
			}
			out, err := runHook(t, cmd, path)
			if err != nil {
				t.Fatalf("hook exited %v; command:\n%s", err, cmd)
			}
			if !strings.Contains(out, "FAKE_GATE_RAN") {
				t.Fatalf("gate binary did not run; stdout %q; command:\n%s", out, cmd)
			}
			args, err := os.ReadFile(filepath.Join(dir, "args"))
			if err != nil {
				t.Fatal(err)
			}
			budget := int((tt.timeout - 30*time.Second) / time.Second)
			want := "gate\n--hook\n--budget\n" + strconv.Itoa(budget) + "s\n"
			if string(args) != want {
				t.Errorf("gate args = %q, want %q", args, want)
			}
		})
	}
}

func TestHookCommandMissingBinaryWarnsAndAllows(t *testing.T) {
	tests := []struct {
		name, bin, wantMsg string
	}{
		{name: "absolute path gone", bin: "/nonexistent/dir/newproj", wantMsg: "done-gate NOT enforced: /nonexistent/dir/newproj is missing"},
		{name: "not on PATH", bin: "", wantMsg: "done-gate NOT enforced: newproj is not on PATH"},
		{name: "path needing JSON escapes", bin: `/tmp/a"b\c/newproj`, wantMsg: `done-gate NOT enforced: /tmp/a"b\c/newproj is missing`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := HookCommand(tt.bin, 1800*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			out, err := runHook(t, cmd, "/usr/bin:/bin")
			if err != nil {
				t.Fatalf("hook exited %v, want 0 (must not block)", err)
			}
			var msg struct {
				SystemMessage string `json:"systemMessage"`
				Decision      string `json:"decision"`
			}
			if err := json.Unmarshal([]byte(out), &msg); err != nil {
				t.Fatalf("stdout is not one JSON object: %q (%v)", out, err)
			}
			if msg.SystemMessage != tt.wantMsg {
				t.Errorf("systemMessage = %q, want %q", msg.SystemMessage, tt.wantMsg)
			}
			if msg.Decision != "" {
				t.Errorf("decision = %q, want none (allow the stop)", msg.Decision)
			}
		})
	}
}

func TestHookCommandRejects(t *testing.T) {
	if _, err := HookCommand("", MinHookTimeout-time.Second); err == nil {
		t.Error("accepted a timeout below the minimum")
	}
	if _, err := HookCommand("bin/newproj", time.Hour); err == nil {
		t.Error("accepted a relative binary path")
	}
}

func TestIsGateHook(t *testing.T) {
	for _, bin := range []string{"", "/Users/me/go/bin/newproj", "/opt/it's here/newproj"} {
		cmd, err := HookCommand(bin, 1800*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !IsGateHook(cmd) {
			t.Errorf("IsGateHook(HookCommand(%q)) = false; command:\n%s", bin, cmd)
		}
	}

	tests := []struct {
		cmd  string
		want bool
	}{
		{"newproj gate --hook", true},
		{"newproj gate --hook --budget 570s", true},
		{"  /usr/local/bin/newproj gate --hook", true},
		{"cd repo && newproj gate --hook", true},
		{"", false},
		{"make check", false},
		{"newproj gate", false},
		{"newproj gate --hookish", false},
		{"newproj new demo", false},
		{"echo newproj gate --hook", false},
		{"echo 'newproj gate --hook'", false},
		{"/usr/bin/not-newproj gate --hook", false},
		{"$CLAUDE_PROJECT_DIR/.claude/hooks/stop-check.sh", false},
	}
	for _, tt := range tests {
		if got := IsGateHook(tt.cmd); got != tt.want {
			t.Errorf("IsGateHook(%q) = %v, want %v", tt.cmd, got, tt.want)
		}
	}
}
