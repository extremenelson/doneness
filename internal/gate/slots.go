package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// SlotsEnv overrides the machine-wide number of concurrent gate checks.
const SlotsEnv = "NEWPROJ_SLOTS"

// MachineConfigFile holds machine-wide gate settings, in configDir:
// {"slots": N}.
const MachineConfigFile = "config.json"

// slotPoll is how often a waiting gate retries the slot locks.
const slotPoll = 100 * time.Millisecond

// SlotCount returns how many gate checks may run at once on this machine:
// $NEWPROJ_SLOTS, else "slots" in the machine config file, else a quarter of
// the CPUs (at least 1). Repositories cannot set it, so the cap holds across
// every repository and worktree.
func SlotCount() (int, error) {
	if v := os.Getenv(SlotsEnv); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return 0, fmt.Errorf("%s=%q: want a positive integer", SlotsEnv, v)
		}
		return n, nil
	}

	dir, err := configDir()
	if err != nil {
		return 0, err
	}
	path := filepath.Join(dir, MachineConfigFile)
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return max(1, runtime.NumCPU()/4), nil
	case err != nil:
		return 0, err
	}
	var mc struct {
		Slots int `json:"slots"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&mc); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	if mc.Slots < 1 {
		return 0, fmt.Errorf("%s: slots must be a positive integer", path)
	}
	return mc.Slots, nil
}

// Slot is one held machine-wide check slot: an exclusive flock on a lock
// file. The lock belongs to the open file, so a child process that inherits
// File keeps the slot held until it (and every process it spawned that kept
// the descriptor) exits, even if the gate process itself is killed.
type Slot struct {
	file *os.File
}

// File is the locked file, to hand to the check process via ExtraFiles.
func (s *Slot) File() *os.File { return s.file }

// Release drops this process's hold on the slot. Child processes that
// inherited the file still hold it until they exit.
func (s *Slot) Release() error { return s.file.Close() }

// AcquireSlot takes one of n slots whose lock files live in lockDir,
// polling until one is free or ctx ends. onWait, if set, is called once,
// the first time no slot is free.
func AcquireSlot(ctx context.Context, lockDir string, n int, onWait func()) (*Slot, error) {
	if n < 1 {
		return nil, fmt.Errorf("slot count %d: want at least 1", n)
	}
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, err
	}
	waited := false
	for {
		for i := range n {
			f, err := os.OpenFile(filepath.Join(lockDir, fmt.Sprintf("slot-%d.lock", i)), os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				return nil, err
			}
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				return &Slot{file: f}, nil
			}
			_ = f.Close()
			if !errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
			}
		}
		if !waited && onWait != nil {
			onWait()
		}
		waited = true
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(slotPoll):
		}
	}
}
