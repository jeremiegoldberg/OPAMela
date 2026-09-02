// Package gitrepo keeps a shallow checkout of a remote git repository in sync.
//
// It shells out to git rather than linking a git implementation: git is already
// a hard requirement of any machine that builds OCaml, and a shallow fetch of
// opam-repository is one process, not a dependency tree.
package gitrepo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repo is a local checkout of a remote repository.
type Repo struct {
	URL string // remote URL
	Dir string // local checkout
}

// Sync makes the checkout exist and match the remote's default branch, then
// reports the resulting revision.
//
// It is safe to call repeatedly: the first call clones, later calls fetch and
// hard-reset. A hard reset is the right tool here because the checkout is a
// cache, not a workspace: nothing local is ever worth keeping.
func (r *Repo) Sync(ctx context.Context) (rev string, err error) {
	if _, err := os.Stat(filepath.Join(r.Dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(r.Dir), 0o755); err != nil {
			return "", err
		}
		if err := os.RemoveAll(r.Dir); err != nil {
			return "", err
		}
		if _, err := run(ctx, "", "git", "clone", "--depth", "1", "--single-branch", r.URL, r.Dir); err != nil {
			return "", fmt.Errorf("cloning %s: %w", r.URL, err)
		}
		return r.Head(ctx)
	}

	// Fetching HEAD by name avoids having to know the default branch, which
	// differs between opam-repository and the overlay repositories.
	if _, err := run(ctx, r.Dir, "git", "fetch", "--depth", "1", "origin", "HEAD"); err != nil {
		return "", fmt.Errorf("fetching %s: %w", r.URL, err)
	}
	if _, err := run(ctx, r.Dir, "git", "reset", "--hard", "FETCH_HEAD"); err != nil {
		return "", fmt.Errorf("resetting %s: %w", r.Dir, err)
	}
	if _, err := run(ctx, r.Dir, "git", "clean", "-fdx"); err != nil {
		return "", fmt.Errorf("cleaning %s: %w", r.Dir, err)
	}
	return r.Head(ctx)
}

// Head reports the revision currently checked out.
func (r *Repo) Head(ctx context.Context) (string, error) {
	out, err := run(ctx, r.Dir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Keep git from blocking on a credential or ssh prompt inside a service.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true")

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return stdout.String(), nil
}
