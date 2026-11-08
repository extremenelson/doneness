package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hookInput(t *testing.T, session, cwd string) *strings.Reader {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"session_id": session, "cwd": cwd, "stop_hook_active": false,
		"hook_event_name": "Stop", "transcript_path": "/dev/null",
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReader(string(b))
}

func hookOpts(state string) HookOptions {
	return HookOptions{Slots: 2, StateDir: state, Version: "test"}
}

func isAllow(r HookResponse) bool { return r.Decision == "" }

func TestHookPassAllows(t *testing.T) {
	isolate(t)
	root := newRepo(t, "true")
	state := t.TempDir()
	resp := Hook(context.Background(), hookInput(t, "s1", root), hookOpts(state))
	if !isAllow(resp) || resp.SystemMessage != "" || resp.Reason != "" {
		t.Fatalf("pass response = %+v, want {}", resp)
	}
	if b, _ := json.Marshal(resp); string(b) != "{}" {
		t.Errorf("pass JSON = %s, want {}", b)
	}
}

func TestHookRedBlocksTwiceThenNotDone(t *testing.T) {
	isolate(t)
	root := newRepo(t, `echo "result: $((40+2))"; exit 1`)
	state := t.TempDir()
	ctx := context.Background()

	for attempt := 1; attempt <= MaxBlocks; attempt++ {
		resp := Hook(ctx, hookInput(t, "s1", root), hookOpts(state))
		if resp.Decision != "block" {
			t.Fatalf("attempt %d = %+v, want block", attempt, resp)
		}
		for _, want := range []string{
			"newproj gate is red (failed)",
			fmt.Sprintf("Attempt %d of 3", attempt),
			"Check: echo \"result:",
			"Last output:\nresult: 42", // the check's output, not its command text
			"check exited 1",
		} {
			if !strings.Contains(resp.Reason, want) {
				t.Errorf("attempt %d reason missing %q:\n%s", attempt, want, resp.Reason)
			}
		}
	}

	third := Hook(ctx, hookInput(t, "s1", root), hookOpts(state))
	if !isAllow(third) || !strings.HasPrefix(third.SystemMessage, "NOT DONE: gate still red after 3 attempts (failed, tree ") {
		t.Fatalf("third red stop = %+v, want allowed with NOT DONE", third)
	}

	// The counter resets after NOT DONE: the next stop blocks again.
	if again := Hook(ctx, hookInput(t, "s1", root), hookOpts(state)); again.Decision != "block" || !strings.Contains(again.Reason, "Attempt 1 of 3") {
		t.Errorf("stop after NOT DONE = %+v, want a fresh attempt 1 block", again)
	}

	// Sessions are counted separately.
	if other := Hook(ctx, hookInput(t, "s2", root), hookOpts(state)); !strings.Contains(other.Reason, "Attempt 1 of 3") {
		t.Errorf("new session = %+v, want attempt 1", other)
	}
}

func TestHookPassResetsCounter(t *testing.T) {
	isolate(t)
	root := newRepo(t, "test -f ok")
	state := t.TempDir()
	ctx := context.Background()
	if r := Hook(ctx, hookInput(t, "s1", root), hookOpts(state)); r.Decision != "block" {
		t.Fatalf("first = %+v, want block", r)
	}
	write(t, filepath.Join(root, "ok"), "")
	if r := Hook(ctx, hookInput(t, "s1", root), hookOpts(state)); !isAllow(r) {
		t.Fatalf("after fix = %+v, want allow", r)
	}
	if err := os.Remove(filepath.Join(root, "ok")); err != nil {
		t.Fatal(err)
	}
	if r := Hook(ctx, hookInput(t, "s1", root), hookOpts(state)); !strings.Contains(r.Reason, "Attempt 1 of 3") {
		t.Errorf("red after a pass = %+v, want the count restarted at 1", r)
	}
}

func TestHookEveryRedKindIsCountedAndBounded(t *testing.T) {
	tests := []struct {
		name, command, kind string
		noConfig            bool
	}{
		{name: "config missing", noConfig: true, kind: "config-error"},
		{name: "command not found", command: "no-such-command-xyz", kind: "config-error"},
		{name: "tree changed", command: "date >> tracked.txt", kind: "tree-changed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			root := newRepo(t, orNone(tt.command))
			if tt.noConfig {
				if err := os.Remove(filepath.Join(root, ConfigFile)); err != nil {
					t.Fatal(err)
				}
			}
			state := t.TempDir()
			var last HookResponse
			for range MaxBlocks + 1 {
				last = Hook(context.Background(), hookInput(t, "s", root), hookOpts(state))
			}
			if !isAllow(last) || !strings.Contains(last.SystemMessage, "NOT DONE") || !strings.Contains(last.SystemMessage, tt.kind) {
				t.Errorf("after %d red stops = %+v, want NOT DONE (%s)", MaxBlocks+1, last, tt.kind)
			}
		})
	}
}

func TestHookTreeChangedNamesFiles(t *testing.T) {
	isolate(t)
	root := newRepo(t, "date >> tracked.txt")
	resp := Hook(context.Background(), hookInput(t, "s", root), hookOpts(t.TempDir()))
	if !strings.Contains(resp.Reason, "Files changed by the check: tracked.txt") {
		t.Errorf("reason does not name the changed file:\n%s", resp.Reason)
	}
}

func TestHookTimeoutBlocks(t *testing.T) {
	isolate(t)
	root := newRepo(t, "sleep 10")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	resp := Hook(ctx, hookInput(t, "s", root), hookOpts(t.TempDir()))
	if resp.Decision != "block" || !strings.Contains(resp.Reason, "timed-out") {
		t.Fatalf("response = %+v, want a timed-out block", resp)
	}
	if el := time.Since(start); el > killGrace {
		t.Errorf("hook took %v past its deadline", el)
	}
}

func TestHookUsesInputCwdNotProcessDir(t *testing.T) {
	isolate(t)
	good := newRepo(t, "true")
	bad := newRepo(t, "exit 1")
	wt := filepath.Join(t.TempDir(), "wt")
	run(t, good, "git", "worktree", "add", "-q", "--detach", wt)
	sub := filepath.Join(wt, "pkg")
	write(t, filepath.Join(sub, "x.txt"), "x\n") // untracked: part of the gated tree

	o := hookOpts(t.TempDir())
	o.FallbackDir = bad // must be ignored when the input has a cwd
	resp := Hook(context.Background(), hookInput(t, "s", sub), o)
	if !isAllow(resp) {
		t.Fatalf("hook from a worktree subdir = %+v, want allow (gate the worktree, not FallbackDir)", resp)
	}

	o.FallbackDir = bad
	if r := Hook(context.Background(), hookInput(t, "s2", ""), o); r.Decision != "block" {
		t.Errorf("no cwd in input = %+v, want FallbackDir (failing repo) to be gated", r)
	}
}

func TestHookBadInputIsBoundedBlock(t *testing.T) {
	for _, body := range []string{"", "not json", "[1,2]"} {
		t.Run(fmt.Sprintf("%q", body), func(t *testing.T) {
			isolate(t)
			state := t.TempDir()
			var last HookResponse
			for i := range MaxBlocks + 1 {
				last = Hook(context.Background(), strings.NewReader(body), hookOpts(state))
				if i < MaxBlocks && (last.Decision != "block" || !strings.Contains(last.Reason, "error")) {
					t.Fatalf("attempt %d = %+v, want an error block", i+1, last)
				}
			}
			if !isAllow(last) || !strings.Contains(last.SystemMessage, "NOT DONE") {
				t.Errorf("bad input never released: %+v", last)
			}
		})
	}
}

func TestHookOutsideRepoIsBoundedBlock(t *testing.T) {
	isolate(t)
	resp := Hook(context.Background(), hookInput(t, "s", t.TempDir()), hookOpts(t.TempDir()))
	if resp.Decision != "block" || !strings.Contains(resp.Reason, "not inside a git repository") {
		t.Errorf("response = %+v, want a block explaining the repo was not found", resp)
	}
}

func TestSessionFileStaysInDir(t *testing.T) {
	for _, id := range []string{"../../etc/passwd", "a/b", "", ".", "..", strings.Repeat("x", 200)} {
		f := sessionFile(id)
		if strings.ContainsAny(f, `/\`) || f == "." || f == ".." || f == "" || len(f) > 128 {
			t.Errorf("sessionFile(%q) = %q escapes or is invalid", id, f)
		}
	}
	if got := sessionFile("9b2f3c1e-abcd-4e5f-8a9b-0123456789ab"); got != "9b2f3c1e-abcd-4e5f-8a9b-0123456789ab" {
		t.Errorf("a UUID session id was rewritten to %q", got)
	}
	if sessionFile("../a") == sessionFile("../b") {
		t.Error("different unsafe ids collide")
	}
}

func TestPruneSessions(t *testing.T) {
	dir := t.TempDir()
	old, fresh := filepath.Join(dir, "old"), filepath.Join(dir, "fresh")
	write(t, old, "1\n")
	write(t, fresh, "1\n")
	past := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	pruneSessions(dir, time.Now())
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("8-day-old session counter was not pruned")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh session counter was pruned")
	}
}

func TestTailWriter(t *testing.T) {
	w := &tailWriter{max: 3}
	_, _ = w.Write([]byte("one\ntwo\nthr"))
	_, _ = w.Write([]byte("ee\nfour\nfive"))
	if got, want := w.String(), "Last output:\nthree\nfour\nfive"; got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}

	long := &tailWriter{max: 2}
	_, _ = long.Write([]byte(strings.Repeat("x", 2000) + "\nend\n"))
	if lines := strings.Split(long.String(), "\n"); len(lines[1]) != maxLineBytes || lines[2] != "end" {
		t.Errorf("long line not capped at %d bytes: %d", maxLineBytes, len(lines[1]))
	}

	if (&tailWriter{max: 3}).String() != "" {
		t.Error("empty tail is not empty")
	}
}
