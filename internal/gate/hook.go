package gate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxBlocks is how many consecutive red stops in one session the hook
	// blocks; the next red stop is allowed with a NOT DONE notice, so a
	// gate that cannot go green never traps the agent.
	MaxBlocks = 2
	// tailLines is how much check output the block reason carries.
	tailLines = 60
	// sessionMaxAge is how long an untouched session counter is kept.
	sessionMaxAge = 7 * 24 * time.Hour
)

// HookInput is the part of Claude Code's Stop-hook stdin the gate uses.
type HookInput struct {
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// HookResponse is the JSON a Stop hook prints on stdout (with exit 0).
// Decision "block" keeps the agent working and sends Reason to it;
// SystemMessage is shown to the user.
type HookResponse struct {
	Decision      string `json:"decision,omitempty"`
	Reason        string `json:"reason,omitempty"`
	SystemMessage string `json:"systemMessage,omitempty"`
}

// HookOptions configures Hook. Root is found from the hook input's cwd;
// FallbackDir is used when the input has none (normally the process cwd).
type HookOptions struct {
	FallbackDir string
	Slots       int    // 0: SlotCount()
	StateDir    string // "": the default state dir
	Version     string // "": Version()
}

// Hook runs the gate as a Claude Code Stop hook and decides whether the
// agent may stop. It never returns an error: every failure becomes a counted
// block, so the hook fails closed but stays bounded (at most MaxBlocks
// blocks in a row per session, then an allowed stop with a NOT DONE notice).
//
// Warnings are put in Reason as well as SystemMessage, because Claude Code
// may not show a systemMessage that accompanies a block.
func Hook(ctx context.Context, stdin io.Reader, o HookOptions) HookResponse {
	state := o.StateDir
	if state == "" {
		var err error
		if state, err = stateDir(); err != nil {
			return HookResponse{Decision: "block", Reason: "doneness: " + err.Error(),
				SystemMessage: "done-gate error: " + err.Error()}
		}
	}
	sessions := filepath.Join(state, "sessions")
	pruneSessions(sessions, time.Now())

	in, inErr := readHookInput(stdin)
	counter := filepath.Join(sessions, sessionFile(in.SessionID))

	dir := in.Cwd
	if dir == "" {
		dir = o.FallbackDir
	}

	var res Result
	var runErr error
	switch {
	case inErr != nil:
		runErr = inErr
	default:
		root, err := TopLevel(ctx, dir)
		if err != nil {
			runErr = err
			break
		}
		out := &tailWriter{max: tailLines}
		res, runErr = Run(ctx, Options{
			Root: root, Out: out, Slots: o.Slots, StateDir: state, Version: o.Version,
		})
		res.Detail = strings.TrimSpace(res.Detail + "\n" + out.String())
	}

	if runErr == nil && res.Outcome.Passed() {
		_ = os.Remove(counter)
		if res.Warning != "" {
			return HookResponse{SystemMessage: "done-gate: " + res.Warning}
		}
		return HookResponse{}
	}

	kind, detail := res.Outcome.String(), res.Detail
	if runErr != nil {
		kind, detail = "error", runErr.Error()
	}
	n := readCount(counter) + 1
	if n > MaxBlocks {
		_ = os.Remove(counter)
		return HookResponse{SystemMessage: fmt.Sprintf(
			"NOT DONE: gate still red after %d attempts (%s, tree %s); this session ended without a passing gate",
			n, kind, orNone(short(res.Tree)))}
	}
	if err := writeCount(counter, n); err != nil {
		detail += "\n(warning: could not record the attempt count: " + err.Error() + ")"
	}

	var reason strings.Builder
	fmt.Fprintf(&reason, "doneness check is red (%s), so this work is not done. Attempt %d of %d; after that the stop is allowed but reported as NOT DONE.\n", kind, n, MaxBlocks+1)
	if res.Command != "" {
		fmt.Fprintf(&reason, "Check: %s\n", res.Command)
	}
	if len(res.Changed) > 0 {
		fmt.Fprintf(&reason, "Files changed by the check: %s\n", strings.Join(res.Changed, ", "))
	}
	if res.Warning != "" {
		fmt.Fprintf(&reason, "Warning: %s\n", res.Warning)
	}
	reason.WriteString(detail)
	reason.WriteString("\nFix the cause, then stop again to re-run the gate.")
	return HookResponse{
		Decision:      "block",
		Reason:        reason.String(),
		SystemMessage: fmt.Sprintf("done-gate red (%s), attempt %d of %d", kind, n, MaxBlocks+1),
	}
}

func readHookInput(r io.Reader) (HookInput, error) {
	var in HookInput
	b, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return in, fmt.Errorf("read hook input: %w", err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return in, errors.New("hook input is empty; expected Claude Code Stop-hook JSON on stdin")
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return in, fmt.Errorf("hook input is not JSON: %w", err)
	}
	return in, nil
}

var safeSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// sessionFile maps a session id to a file name; anything that is not a
// plain id (path separators, dots, empty) is hashed so it cannot escape the
// sessions directory.
func sessionFile(id string) string {
	if safeSessionID.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "h-" + hex.EncodeToString(sum[:16])
}

func readCount(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func writeCount(path string, n int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(n)+"\n"), 0o644)
}

// pruneSessions removes session counters untouched for sessionMaxAge.
func pruneSessions(dir string, now time.Time) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil && now.Sub(fi.ModTime()) > sessionMaxAge {
			_ = os.Remove(path)
		}
		return nil
	})
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// tailWriter keeps the last max lines written to it, each cut to 500 bytes.
type tailWriter struct {
	max     int
	lines   []string
	partial []byte
}

const maxLineBytes = 500

func (w *tailWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.partial = appendCapped(w.partial, p)
			break
		}
		w.partial = appendCapped(w.partial, p[:i])
		w.push(string(w.partial))
		w.partial = w.partial[:0]
		p = p[i+1:]
	}
	return n, nil
}

func appendCapped(dst, src []byte) []byte {
	if room := maxLineBytes - len(dst); room < len(src) {
		src = src[:max(room, 0)]
	}
	return append(dst, src...)
}

func (w *tailWriter) push(line string) {
	w.lines = append(w.lines, line)
	if len(w.lines) > w.max {
		w.lines = w.lines[len(w.lines)-w.max:]
	}
}

// String returns the kept lines under a "Last output:" heading, or "" if
// nothing was written.
func (w *tailWriter) String() string {
	lines := w.lines
	if len(w.partial) > 0 {
		lines = append(append([]string{}, lines...), string(w.partial))
		if len(lines) > w.max {
			lines = lines[len(lines)-w.max:]
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "Last output:\n" + strings.Join(lines, "\n")
}
