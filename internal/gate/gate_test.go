package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// isolate points git and the XDG directories at temp dirs so tests never
// read the developer's git config or touch their real gate state.
func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	cfg := filepath.Join(home, "gitconfig")
	body := "[user]\n\tname = gate test\n\temail = test@example.invalid\n[commit]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv(SlotsEnv, "")
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo creates a committed repository whose gate runs command.
func newRepo(t *testing.T, command string) string {
	t.Helper()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	run(t, root, "git", "init", "-q")
	write(t, filepath.Join(root, "tracked.txt"), "v1\n")
	write(t, filepath.Join(root, ".gitignore"), "ignored*\nbuild/\n")
	if err := WriteConfig(root, Config{Gate: Gate{Command: command}}); err != nil {
		t.Fatal(err)
	}
	run(t, root, "git", "add", "-A")
	run(t, root, "git", "commit", "-q", "-m", "init")
	return root
}

func opts(root string) Options {
	return Options{Root: root, Slots: 4, Version: "test"}
}

func mustRun(t *testing.T, ctx context.Context, o Options) Result {
	t.Helper()
	res, err := Run(ctx, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func hash(t *testing.T, root string) string {
	t.Helper()
	h, err := TreeHash(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestTreeHash(t *testing.T) {
	isolate(t)
	root := newRepo(t, "true")
	statusBefore := run(t, root, "git", "status", "--porcelain")
	base := hash(t, root)
	if base != run(t, root, "git", "rev-parse", "HEAD^{tree}") {
		t.Error("clean tree hash differs from HEAD's tree")
	}

	write(t, filepath.Join(root, "ignored.log"), "noise\n")
	if hash(t, root) != base {
		t.Error("an ignored untracked file changed the hash")
	}

	write(t, filepath.Join(root, "new.txt"), "x\n")
	untracked := hash(t, root)
	if untracked == base {
		t.Error("an untracked file did not change the hash")
	}

	write(t, filepath.Join(root, "tracked.txt"), "v2\n")
	if hash(t, root) == untracked {
		t.Error("a tracked modification did not change the hash")
	}

	if got := run(t, root, "git", "diff", "--cached", "--name-only"); got != "" {
		t.Errorf("TreeHash staged files in the real index: %q", got)
	}
	_ = statusBefore
}

func TestTreeHashTrackedButIgnored(t *testing.T) {
	isolate(t)
	root := newRepo(t, "true")
	write(t, filepath.Join(root, "ignored-but-tracked.txt"), "a\n")
	run(t, root, "git", "add", "-f", "ignored-but-tracked.txt")
	run(t, root, "git", "commit", "-q", "-m", "force-add")
	before := hash(t, root)
	write(t, filepath.Join(root, "ignored-but-tracked.txt"), "b\n")
	if hash(t, root) == before {
		t.Error("a change to a tracked file matching .gitignore did not change the hash")
	}
}

func TestTreeHashLinkedWorktreeParity(t *testing.T) {
	isolate(t)
	root := newRepo(t, "true")
	wt := filepath.Join(t.TempDir(), "wt")
	run(t, root, "git", "worktree", "add", "-q", "--detach", wt)
	write(t, filepath.Join(root, "extra.txt"), "same\n")
	write(t, filepath.Join(wt, "extra.txt"), "same\n")
	if a, b := hash(t, root), hash(t, wt); a != b {
		t.Errorf("same state hashes differently: main %s, worktree %s", a, b)
	}
}

func TestTreeHashNoCommits(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	run(t, root, "git", "init", "-q")
	write(t, filepath.Join(root, "a.txt"), "a\n")
	if h := hash(t, root); len(h) != 40 && len(h) != 64 {
		t.Errorf("hash %q is not an object id", h)
	}
}

func TestSlotCount(t *testing.T) {
	isolate(t)
	if n, err := SlotCount(); err != nil || n != max(1, runtime.NumCPU()/4) {
		t.Errorf("default = %d, %v; want NumCPU/4", n, err)
	}

	mc := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "newproj", MachineConfigFile)
	write(t, mc, `{"slots": 3}`)
	if n, err := SlotCount(); err != nil || n != 3 {
		t.Errorf("from config = %d, %v; want 3", n, err)
	}

	t.Setenv(SlotsEnv, "5")
	if n, err := SlotCount(); err != nil || n != 5 {
		t.Errorf("env over config = %d, %v; want 5", n, err)
	}
	for _, bad := range []string{"0", "-1", "two"} {
		t.Setenv(SlotsEnv, bad)
		if _, err := SlotCount(); err == nil {
			t.Errorf("%s=%q accepted", SlotsEnv, bad)
		}
	}
	t.Setenv(SlotsEnv, "")

	for _, body := range []string{`{"slots": 0}`, `{"slot": 2}`, `not json`} {
		write(t, mc, body)
		if _, err := SlotCount(); err == nil {
			t.Errorf("machine config %s accepted", body)
		}
	}
}

func TestOutcomeExitCodes(t *testing.T) {
	want := map[Outcome]int{Pass: 0, Cached: 0, Failed: 1, ConfigError: 2, TreeChanged: 3, TimedOut: 4, Interrupted: 130}
	for o, code := range want {
		if got := o.ExitCode(); got != code {
			t.Errorf("%s.ExitCode() = %d, want %d", o, got, code)
		}
	}
}

func TestRunOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    Outcome
		code    int
		changed []string
	}{
		{name: "pass", command: "true", want: Pass},
		{name: "compound pass", command: "true && test -f tracked.txt", want: Pass},
		{name: "runs at top level", command: "test -f .newproj.json", want: Pass},
		{name: "failure keeps exit code", command: "exit 3", want: Failed, code: 3},
		{name: "command not found", command: "definitely-not-a-command-xyz", want: ConfigError, code: 127},
		{name: "CI=1 is set", command: `test "$CI" = 1`, want: Pass},
		{name: "stdin is empty", command: "! read -r line", want: Pass},
		{name: "writes a tracked file", command: "echo changed >> tracked.txt", want: TreeChanged, changed: []string{"tracked.txt"}},
		{name: "creates an untracked file", command: "echo x > generated.txt", want: TreeChanged, changed: []string{"generated.txt"}},
		{name: "writes only ignored files", command: "mkdir -p build && echo x > build/out", want: Pass},
		{name: "fails and writes: guard wins", command: "echo x >> tracked.txt; exit 1", want: TreeChanged, changed: []string{"tracked.txt"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			root := newRepo(t, tt.command)
			res := mustRun(t, context.Background(), opts(root))
			if res.Outcome != tt.want {
				t.Fatalf("outcome = %s (%s), want %s", res.Outcome, res.Detail, tt.want)
			}
			if tt.code != 0 && res.ExitCode != tt.code {
				t.Errorf("check exit code = %d, want %d", res.ExitCode, tt.code)
			}
			if tt.changed != nil && strings.Join(res.Changed, ",") != strings.Join(tt.changed, ",") {
				t.Errorf("changed = %q, want %q", res.Changed, tt.changed)
			}
		})
	}
}

func TestRunCIAlreadySet(t *testing.T) {
	isolate(t)
	t.Setenv("CI", "true")
	root := newRepo(t, `test "$CI" = true`)
	if res := mustRun(t, context.Background(), opts(root)); res.Outcome != Pass {
		t.Fatalf("outcome = %s (%s): an existing CI value was overwritten", res.Outcome, res.Detail)
	}
}

func TestRunMissingConfig(t *testing.T) {
	isolate(t)
	root := newRepo(t, "true")
	if err := os.Remove(filepath.Join(root, ConfigFile)); err != nil {
		t.Fatal(err)
	}
	res := mustRun(t, context.Background(), opts(root))
	if res.Outcome != ConfigError || !strings.Contains(res.Detail, "newproj adopt") {
		t.Fatalf("outcome = %s (%q), want config error naming adopt", res.Outcome, res.Detail)
	}
}

func TestReceiptsAndCache(t *testing.T) {
	isolate(t)
	counter := filepath.Join(t.TempDir(), "runs")
	root := newRepo(t, "echo run >> "+counter)
	runs := func() int {
		b, _ := os.ReadFile(counter)
		return strings.Count(string(b), "run")
	}
	ctx := context.Background()

	first := mustRun(t, ctx, opts(root))
	if first.Outcome != Pass {
		t.Fatalf("first run = %s (%s)", first.Outcome, first.Detail)
	}
	r, err := ReadReceipt(ctx, root, first.Tree)
	if err != nil || r == nil {
		t.Fatalf("no receipt after pass: %v", err)
	}
	if r.Tree != first.Tree || r.Command != "echo run >> "+counter || r.Version != "test" ||
		r.GOOS != runtime.GOOS || r.GOARCH != runtime.GOARCH || r.ConfigHash == "" || r.At.IsZero() {
		t.Errorf("receipt fields wrong: %+v", r)
	}

	if res := mustRun(t, ctx, opts(root)); res.Outcome != Cached || runs() != 1 {
		t.Errorf("second run = %s with %d check runs, want cached and 1", res.Outcome, runs())
	}

	o := opts(root)
	o.NoCache = true
	if res := mustRun(t, ctx, o); res.Outcome != Pass || runs() != 2 {
		t.Errorf("--no-cache run = %s with %d check runs, want pass and 2", res.Outcome, runs())
	}

	o = opts(root)
	o.Version = "other"
	if res := mustRun(t, ctx, o); res.Outcome != Pass || runs() != 3 {
		t.Errorf("new gate version = %s with %d runs, want a fresh pass", res.Outcome, runs())
	}

	write(t, filepath.Join(root, "tracked.txt"), "v2\n")
	if res := mustRun(t, ctx, opts(root)); res.Outcome != Pass || runs() != 4 {
		t.Errorf("edited tree = %s with %d runs, want a fresh pass", res.Outcome, runs())
	}

	// Changing the command changes the config hash (and the tree, since the
	// config file is part of it): never satisfied by the old receipt.
	if err := WriteConfig(root, Config{Gate: Gate{Command: "echo run >> " + counter + " # v2"}}); err != nil {
		t.Fatal(err)
	}
	if res := mustRun(t, ctx, opts(root)); res.Outcome != Pass || runs() != 5 {
		t.Errorf("new command = %s with %d runs, want a fresh pass", res.Outcome, runs())
	}
}

func TestReceiptMustMatchConfig(t *testing.T) {
	cfg := Config{Gate: Gate{Command: "make check"}}
	ok := Receipt{Command: "make check", ConfigHash: cfg.Hash(), Version: "v1", GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	if !ok.satisfies(cfg, "v1") {
		t.Fatal("matching receipt rejected")
	}
	for name, mutate := range map[string]func(*Receipt){
		"command": func(r *Receipt) { r.Command = "make test" },
		"hash":    func(r *Receipt) { r.ConfigHash = "deadbeef" },
		"version": func(r *Receipt) { r.Version = "v2" },
		"goos":    func(r *Receipt) { r.GOOS = "plan9" },
		"goarch":  func(r *Receipt) { r.GOARCH = "mips" },
	} {
		r := ok
		mutate(&r)
		if r.satisfies(cfg, "v1") {
			t.Errorf("receipt with different %s accepted", name)
		}
	}
}

func TestFailureWritesLastRedNotReceipt(t *testing.T) {
	isolate(t)
	root := newRepo(t, "exit 1")
	state := t.TempDir()
	o := opts(root)
	o.StateDir = state
	res := mustRun(t, context.Background(), o)
	if res.Outcome != Failed {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	if r, err := ReadReceipt(context.Background(), root, res.Tree); err != nil || r != nil {
		t.Errorf("failed run left a receipt (%+v, %v)", r, err)
	}
	lr, err := ReadLastRed(state, res.Tree)
	if err != nil || lr == nil || lr.Kind != "failed" {
		t.Errorf("last red = %+v, %v; want kind failed", lr, err)
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	isolate(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	root := newRepo(t, "sleep 30 & echo $! > "+pidFile+"; wait")
	state := t.TempDir()
	o := opts(root)
	o.StateDir = state

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := mustRun(t, ctx, o)
	if res.Outcome != TimedOut {
		t.Fatalf("outcome = %s (%s), want timed out", res.Outcome, res.Detail)
	}
	if el := time.Since(start); el > killGrace {
		t.Errorf("timed-out run took %v", el)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("grandchild sleep (pid %d) survived the timeout (kill -0: %v)", pid, err)
	}
	if lr, _ := ReadLastRed(state, res.Tree); lr == nil || lr.Kind != "timed-out" {
		t.Errorf("last red = %+v, want timed-out", lr)
	}
}

func TestLeftoverBackgroundProcessKilled(t *testing.T) {
	isolate(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	root := newRepo(t, "sleep 30 & echo $! > "+pidFile)
	start := time.Now()
	if res := mustRun(t, context.Background(), opts(root)); res.Outcome != Pass {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	if el := time.Since(start); el > killGrace {
		t.Errorf("gate waited %v for a background process the check left behind", el)
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("background process left by the check survived (pid %d)", pid)
	}
}

func TestSlotWaitTimeout(t *testing.T) {
	isolate(t)
	root := newRepo(t, "true")
	state := t.TempDir()
	held, err := AcquireSlot(context.Background(), filepath.Join(state, "locks"), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	o := opts(root)
	o.Slots, o.StateDir = 1, state
	var log bytes.Buffer
	o.Log = &log
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res := mustRun(t, ctx, o)
	if res.Outcome != TimedOut || !strings.Contains(res.Detail, "waiting") {
		t.Fatalf("outcome = %s (%s), want timed out waiting", res.Outcome, res.Detail)
	}
	if !strings.Contains(log.String(), "waiting for slot") {
		t.Errorf("log = %q, want a waiting line", log.String())
	}
	if lr, _ := ReadLastRed(state, res.Tree); lr != nil {
		t.Errorf("slot-wait timeout recorded a failure for an untested tree: %+v", lr)
	}
}

// syncWriter signals once its contents contain want.
type syncWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	want string
	hit  chan struct{}
	once sync.Once
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.want) {
		w.once.Do(func() { close(w.hit) })
	}
	return n, err
}

func TestEditWhileWaitingIsNotAMutation(t *testing.T) {
	isolate(t)
	root := newRepo(t, "true")
	state := t.TempDir()
	held, err := AcquireSlot(context.Background(), filepath.Join(state, "locks"), 1, nil)
	if err != nil {
		t.Fatal(err)
	}

	o := opts(root)
	o.Slots, o.StateDir = 1, state
	sw := &syncWriter{want: "waiting for slot", hit: make(chan struct{})}
	o.Log = sw
	done := make(chan Result, 1)
	go func() {
		res, err := Run(context.Background(), o)
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()

	select {
	case <-sw.hit:
	case <-time.After(10 * time.Second):
		t.Fatal("gate never waited for the slot")
	}
	write(t, filepath.Join(root, "tracked.txt"), "edited while waiting\n")
	edited := hash(t, root)
	_ = held.Release()

	res := <-done
	if res.Outcome != Pass {
		t.Fatalf("outcome = %s (%s), want pass", res.Outcome, res.Detail)
	}
	if res.Tree != edited {
		t.Error("receipt is not keyed to the tree that was actually tested")
	}
}

func TestMachineWideSlotCap(t *testing.T) {
	isolate(t)
	markers := t.TempDir()
	maxFile := filepath.Join(t.TempDir(), "max")
	// Each check counts the checks running right now (itself included).
	check := fmt.Sprintf(`touch %[1]s/run-$$; ls %[1]s | wc -l >> %[2]s; sleep 0.4; rm %[1]s/run-$$`, markers, maxFile)
	repos := []string{newRepo(t, check), newRepo(t, check)}
	state := t.TempDir()

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := opts(repos[i%2])
			o.Slots, o.StateDir, o.NoCache = 2, state, true
			res, err := Run(context.Background(), o)
			if err != nil || res.Outcome != Pass {
				t.Errorf("run %d = %s, %v", i, res.Outcome, err)
			}
		}()
	}
	wg.Wait()

	b, err := os.ReadFile(maxFile)
	if err != nil {
		t.Fatal(err)
	}
	counts := strings.Fields(string(b))
	if len(counts) != 4 {
		t.Fatalf("recorded %d checks, want 4", len(counts))
	}
	for _, c := range counts {
		if n, _ := strconv.Atoi(c); n > 2 {
			t.Fatalf("%d checks ran at once with 2 slots (counts %v)", n, counts)
		}
	}
}

func TestConcurrentWorktreeReceipts(t *testing.T) {
	isolate(t)
	root := newRepo(t, "true")
	var wts []string
	for i := range 4 {
		wt := filepath.Join(t.TempDir(), fmt.Sprintf("wt%d", i))
		run(t, root, "git", "worktree", "add", "-q", "--detach", wt)
		write(t, filepath.Join(wt, "tracked.txt"), fmt.Sprintf("worktree %d\n", i))
		wts = append(wts, wt)
	}
	state := t.TempDir()
	results := make([]Result, len(wts))
	var wg sync.WaitGroup
	for i, wt := range wts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := opts(wt)
			o.StateDir = state
			res, err := Run(context.Background(), o)
			if err != nil {
				t.Error(err)
			}
			results[i] = res
		}()
	}
	wg.Wait()
	for i, res := range results {
		if res.Outcome != Pass || res.Warning != "" {
			t.Errorf("worktree %d = %s, warning %q", i, res.Outcome, res.Warning)
			continue
		}
		if r, err := ReadReceipt(context.Background(), root, res.Tree); err != nil || r == nil {
			t.Errorf("worktree %d receipt missing (%v)", i, err)
		}
	}
}

// TestKilledGateKeepsSlotHeld: when the gate process is SIGKILLed mid-check
// (no cleanup runs), the check inherits the slot lock, so the machine-wide
// cap holds until the check's processes exit.
func TestKilledGateKeepsSlotHeld(t *testing.T) {
	if os.Getenv("GATE_HELPER_ROOT") != "" {
		helperGate()
		return
	}
	isolate(t)
	pids := t.TempDir()
	root := newRepo(t, fmt.Sprintf("echo $$ > %[1]s/sh; sleep 30 & echo $! > %[1]s/sleep; wait", pids))
	state := t.TempDir()

	helper := exec.Command(os.Args[0], "-test.run=^TestKilledGateKeepsSlotHeld$")
	helper.Env = append(os.Environ(), "GATE_HELPER_ROOT="+root, "GATE_HELPER_STATE="+state)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	sleepPid := waitPid(t, filepath.Join(pids, "sleep"))
	shPid := waitPid(t, filepath.Join(pids, "sh"))
	t.Cleanup(func() {
		_ = syscall.Kill(sleepPid, syscall.SIGKILL)
		_ = syscall.Kill(shPid, syscall.SIGKILL)
	})

	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()

	lockDir := filepath.Join(state, "locks")
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if s, err := AcquireSlot(ctx, lockDir, 1, nil); err == nil {
		_ = s.Release()
		t.Fatal("slot was free while the killed gate's check was still running")
	}

	_ = syscall.Kill(sleepPid, syscall.SIGKILL)
	_ = syscall.Kill(shPid, syscall.SIGKILL)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	s, err := AcquireSlot(ctx2, lockDir, 1, nil)
	if err != nil {
		t.Fatalf("slot never freed after the check exited: %v", err)
	}
	_ = s.Release()
}

// helperGate runs in the child process of TestKilledGateKeepsSlotHeld.
func helperGate() {
	o := Options{Root: os.Getenv("GATE_HELPER_ROOT"), Slots: 1, StateDir: os.Getenv("GATE_HELPER_STATE"), Version: "test"}
	_, _ = Run(context.Background(), o)
	os.Exit(0)
}

func waitPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", path)
	return 0
}

// TestCachedPassIsFast: re-gating an unchanged tree of 10k files must not
// rerun the check and must answer quickly. Slow to set up: skipped with -short.
func TestCachedPassIsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a 10k-file repository")
	}
	isolate(t)
	root := newRepo(t, "true")
	for i := range 10_000 {
		write(t, filepath.Join(root, "files", fmt.Sprintf("d%02d", i%100), fmt.Sprintf("f%05d.txt", i)), strconv.Itoa(i))
	}
	run(t, root, "git", "add", "-A")
	run(t, root, "git", "commit", "-q", "-m", "10k files")
	ctx := context.Background()
	if res := mustRun(t, ctx, opts(root)); res.Outcome != Pass {
		t.Fatalf("first run = %s", res.Outcome)
	}
	start := time.Now()
	res := mustRun(t, ctx, opts(root))
	el := time.Since(start)
	if res.Outcome != Cached {
		t.Fatalf("second run = %s, want cached", res.Outcome)
	}
	t.Logf("cached pass on 10k files took %v", el)
	if el > time.Second {
		t.Errorf("cached pass took %v, want under 1s", el)
	}
}
