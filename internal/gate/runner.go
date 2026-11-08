package gate

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	// killGrace is how long the check gets after SIGTERM before SIGKILL.
	killGrace = 5 * time.Second
	// pipeGrace is how long to keep reading output after the shell exits.
	pipeGrace = 500 * time.Millisecond
)

// checkResult is how a check process ended.
type checkResult struct {
	exitCode int
	stopped  error // context.DeadlineExceeded or context.Canceled if ctx ended it
}

// runCheck runs command with `sh -c` in root, in its own process group, with
// CI=1 (unless already set) and stdin from the null device so tools take
// their non-interactive path. slot is inherited by the check so the slot stays
// held until every process in the group has exited.
//
// When ctx ends, the whole group gets SIGTERM, then SIGKILL after killGrace.
// After the shell exits, any processes it left in the group are killed too:
// a verify-only check has no business leaving work running.
func runCheck(ctx context.Context, root, command string, slot *os.File, out io.Writer) (checkResult, error) {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = root
	cmd.Env = os.Environ()
	if _, ok := os.LookupEnv("CI"); !ok {
		cmd.Env = append(cmd.Env, "CI=1")
	}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.ExtraFiles = []*os.File{slot}
	cmd.SysProcAttr = sysProcAttr()
	// A background process the check leaves behind keeps the output pipe
	// open; without a bound, Wait would block until that process exits.
	cmd.WaitDelay = pipeGrace

	if err := cmd.Start(); err != nil {
		return checkResult{}, err
	}
	pgid := cmd.Process.Pid // Setpgid: the shell leads its own group

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var stopped error
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		stopped = ctx.Err()
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case waitErr = <-done:
		case <-time.After(killGrace):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			waitErr = <-done
		}
	}
	// Reap anything the shell left behind in its group (ESRCH when empty).
	_ = syscall.Kill(-pgid, syscall.SIGKILL)

	res := checkResult{stopped: stopped}
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil, errors.Is(waitErr, exec.ErrWaitDelay):
		// ErrWaitDelay: the shell exited 0 but left a process holding the
		// output pipe; that process was killed above.
	case errors.As(waitErr, &exitErr):
		res.exitCode = exitErr.ExitCode()
	default:
		return res, waitErr
	}
	return res, nil
}
