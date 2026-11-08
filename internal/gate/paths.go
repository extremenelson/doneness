package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// stateDir is where machine-local gate state lives (slot locks, last red
// outcomes): $XDG_STATE_HOME/newproj, else ~/.local/state/newproj.
func stateDir() (string, error) {
	return xdgDir("XDG_STATE_HOME", ".local/state")
}

// configDir is where machine-wide gate settings live: $XDG_CONFIG_HOME/newproj,
// else ~/.config/newproj.
func configDir() (string, error) {
	return xdgDir("XDG_CONFIG_HOME", ".config")
}

func xdgDir(env, fallback string) (string, error) {
	if base := os.Getenv(env); filepath.IsAbs(base) {
		return filepath.Join(base, "newproj"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory for %s: %w", env, err)
	}
	return filepath.Join(home, fallback, "newproj"), nil
}

// gitError carries a failed git command's stderr.
type gitError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, strings.TrimSpace(e.stderr))
}

func (e *gitError) Unwrap() error { return e.err }

// git runs git in dir with extra environment and optional stdin, returning
// trimmed stdout.
func git(ctx context.Context, dir string, env []string, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdin
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", &gitError{args: args, stderr: stderr.String(), err: err}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// TopLevel returns the absolute top-level directory of the work tree that
// contains dir.
func TopLevel(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, nil, nil, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		var ge *gitError
		if errors.As(err, &ge) && strings.Contains(ge.stderr, "not a git repository") {
			return "", fmt.Errorf("%s is not inside a git repository", dir)
		}
		return "", err
	}
	return out, nil
}
