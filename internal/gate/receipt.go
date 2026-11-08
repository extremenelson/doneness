package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// NotesRef is the git notes ref that holds PASS receipts, one note per
// tested tree.
const NotesRef = "refs/notes/newproj-gate"

// Receipt records that a tree passed the gate. Only passes are recorded in
// git; the latest failure per tree is kept in local state (see LastRed).
type Receipt struct {
	Tree       string    `json:"tree"`
	Command    string    `json:"command"`
	ConfigHash string    `json:"config_hash"`
	DurationMS int64     `json:"duration_ms"`
	At         time.Time `json:"at"`
	Host       string    `json:"host"`
	GOOS       string    `json:"goos"`
	GOARCH     string    `json:"goarch"`
	Version    string    `json:"newproj_version"`
}

// satisfies reports whether r proves cfg for this platform and gate version,
// so a later gate on the same tree can skip the check.
func (r Receipt) satisfies(cfg Config, version string) bool {
	return r.Command == cfg.Gate.Command &&
		r.ConfigHash == cfg.Hash() &&
		r.Version == version &&
		r.GOOS == runtime.GOOS &&
		r.GOARCH == runtime.GOARCH
}

// ReadReceipt returns the PASS receipt for tree, or nil if there is none.
func ReadReceipt(ctx context.Context, root, tree string) (*Receipt, error) {
	out, err := git(ctx, root, nil, nil, "notes", "--ref="+NotesRef, "show", tree)
	if err != nil {
		var ge *gitError
		if errors.As(err, &ge) && strings.Contains(ge.stderr, "no note found") {
			return nil, nil
		}
		return nil, err
	}
	var r Receipt
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return nil, fmt.Errorf("receipt for %s is not valid JSON: %w", tree, err)
	}
	return &r, nil
}

// receiptRetries and receiptBackoff bound retries on notes-ref lock
// contention (worktrees of one repository share the ref): 50ms doubling,
// five retries.
const (
	receiptRetries = 5
	receiptBackoff = 50 * time.Millisecond
)

// writeReceipt records a PASS receipt and reads it back.
//
// `git notes add` reads the notes ref, builds a new notes commit and moves
// the ref without checking it is unchanged, so two concurrent writers can
// silently lose one note. Writers in one repository (all its worktrees share
// the git common dir) are therefore serialized with a flock there; retries
// still cover ref-lock contention from other git commands.
func writeReceipt(ctx context.Context, root string, r Receipt) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	unlock, err := lockNotes(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()

	delay := receiptBackoff
	for attempt := 0; ; attempt++ {
		_, err = git(ctx, root, nil, strings.NewReader(string(body)),
			"notes", "--ref="+NotesRef, "add", "-f", "-F", "-", r.Tree)
		if err == nil || attempt == receiptRetries || !isLockContention(err) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	if err != nil {
		return err
	}

	got, err := ReadReceipt(ctx, root, r.Tree)
	switch {
	case err != nil:
		return fmt.Errorf("read back receipt: %w", err)
	case got == nil || got.At != r.At || got.Command != r.Command:
		return errors.New("receipt was not stored (read-back mismatch)")
	}
	return nil
}

// lockNotes takes an exclusive flock shared by every worktree of the
// repository at root, and returns its release function.
func lockNotes(ctx context.Context, root string) (func(), error) {
	common, err := git(ctx, root, nil, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(common, "newproj-gate-notes.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

func isLockContention(err error) bool {
	var ge *gitError
	if !errors.As(err, &ge) {
		return false
	}
	s := ge.stderr
	return strings.Contains(s, "cannot lock ref") || strings.Contains(s, ".lock': File exists") ||
		strings.Contains(s, "Unable to create") && strings.Contains(s, ".lock")
}

// LastRed is the latest failed gate outcome recorded for a tree on this
// machine.
type LastRed struct {
	Tree string    `json:"tree"`
	Kind string    `json:"kind"` // Outcome.String() of the failure
	At   time.Time `json:"at"`
}

func lastRedPath(stateDir, tree string) string {
	return filepath.Join(stateDir, "last", tree+".json")
}

func writeLastRed(stateDir string, lr LastRed) error {
	path := lastRedPath(stateDir, lr.Tree)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(lr)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ReadLastRed returns the latest failure recorded for tree, or nil.
func ReadLastRed(stateDir, tree string) (*LastRed, error) {
	b, err := os.ReadFile(lastRedPath(stateDir, tree))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lr LastRed
	if err := json.Unmarshal(b, &lr); err != nil {
		return nil, err
	}
	return &lr, nil
}
