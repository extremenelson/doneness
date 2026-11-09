package gate

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

// TreeHash returns the git tree id of the work tree rooted at root as it is
// on disk right now: tracked files (including tracked files that match
// .gitignore), modified files and untracked files that are not ignored.
//
// It stages into a temporary copy of the real index, so the real index is
// never modified, and git's stat cache in the copy keeps unchanged files from
// being re-hashed.
func TreeHash(ctx context.Context, root string) (string, error) {
	index, err := git(ctx, root, nil, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp("", "doneness-index-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	var srcMtime time.Time
	if src, err := os.Open(index); err == nil {
		fi, serr := src.Stat()
		_, cerr := io.Copy(tmp, src)
		_ = src.Close()
		if err := errors.Join(serr, cerr); err != nil {
			_ = tmp.Close()
			return "", err
		}
		srcMtime = fi.ModTime()
	} else if !errors.Is(err, fs.ErrNotExist) {
		_ = tmp.Close()
		return "", err
	}
	// A repository with no commits has no index yet: git treats an empty
	// file as an invalid index, so remove it and let git start fresh.
	fi, err := tmp.Stat()
	if err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if fi.Size() == 0 {
		if err := os.Remove(tmpPath); err != nil {
			return "", err
		}
	} else if err := os.Chtimes(tmpPath, srcMtime, srcMtime); err != nil {
		// Git trusts an entry's cached stat data only if the file's mtime
		// is older than the index file's own mtime ("racy git"). A fresh
		// copy has mtime now, which would make git trust stale entries for
		// files rewritten (same size) within the same second; keep the
		// original index's mtime so the racy check still works.
		return "", err
	}

	env := []string{"GIT_INDEX_FILE=" + tmpPath}
	if _, err := git(ctx, root, env, nil, "add", "--all", "--", "."); err != nil {
		return "", err
	}
	return git(ctx, root, env, nil, "write-tree")
}

// changedPaths lists the paths that differ between two trees.
func changedPaths(ctx context.Context, root, from, to string) ([]string, error) {
	out, err := git(ctx, root, nil, nil, "diff-tree", "-r", "--name-only", "--no-commit-id", from, to)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}
