package gitrepo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newUpstream builds a real git repository on disk to clone from, so these
// tests exercise git itself without touching the network.
func newUpstream(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	commit(t, dir, "init")
	return dir
}

func commit(t *testing.T, dir, message string) {
	t.Helper()

	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		for _, args := range [][]string{
			{"init", "--initial-branch=main"},
			{"config", "user.email", "test@example.org"},
			{"config", "user.name", "test"},
		} {
			mustGit(t, dir, args...)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "repo"), []byte(`opam-version: "2.0"`+"\n"+message+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-m", message)
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestSyncClonesThenUpdates(t *testing.T) {
	upstream := newUpstream(t)
	r := &Repo{URL: upstream, Dir: filepath.Join(t.TempDir(), "checkout")}
	ctx := context.Background()

	first, err := r.Sync(ctx)
	if err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if first == "" {
		t.Fatal("first Sync returned an empty revision")
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "repo")); err != nil {
		t.Fatalf("clone did not produce a checkout: %v", err)
	}

	// Nothing moved upstream: the revision must be stable, which is what the
	// refresh loop relies on to skip a rebuild.
	again, err := r.Sync(ctx)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if again != first {
		t.Errorf("revision changed without an upstream commit: %s then %s", first, again)
	}

	commit(t, upstream, "a new package")
	third, err := r.Sync(ctx)
	if err != nil {
		t.Fatalf("third Sync: %v", err)
	}
	if third == first {
		t.Error("revision did not change after an upstream commit")
	}

	got, err := os.ReadFile(filepath.Join(r.Dir, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "a new package"; !strings.Contains(string(got), want) {
		t.Errorf("checkout content = %q, want it to mention %q", got, want)
	}
}

// The checkout is a cache, not a workspace: local edits must not survive and
// must never block a refresh.
func TestSyncDiscardsLocalChanges(t *testing.T) {
	upstream := newUpstream(t)
	r := &Repo{URL: upstream, Dir: filepath.Join(t.TempDir(), "checkout")}
	ctx := context.Background()

	if _, err := r.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	tracked := filepath.Join(r.Dir, "repo")
	if err := os.WriteFile(tracked, []byte("hand edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	untracked := filepath.Join(r.Dir, "stray-file")
	if err := os.WriteFile(untracked, []byte("left behind\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	commit(t, upstream, "second")
	if _, err := r.Sync(ctx); err != nil {
		t.Fatalf("Sync after local edits: %v", err)
	}

	got, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "hand edited") {
		t.Error("a local edit survived a sync")
	}
	if _, err := os.Stat(untracked); err == nil {
		t.Error("an untracked file survived a sync")
	}
}

func TestSyncRepairsABrokenCheckout(t *testing.T) {
	upstream := newUpstream(t)
	dir := filepath.Join(t.TempDir(), "checkout")
	r := &Repo{URL: upstream, Dir: dir}
	ctx := context.Background()

	// A directory that exists but is not a repository, as left by a killed
	// clone, must be replaced rather than reported forever.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Sync(ctx); err != nil {
		t.Fatalf("Sync over a non-repository directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Errorf("no repository after Sync: %v", err)
	}
}

func TestSyncFailsOnAMissingRemote(t *testing.T) {
	r := &Repo{URL: filepath.Join(t.TempDir(), "does-not-exist"), Dir: filepath.Join(t.TempDir(), "checkout")}
	if _, err := r.Sync(context.Background()); err == nil {
		t.Fatal("err = nil, want a failure for an unreachable remote")
	}
}
