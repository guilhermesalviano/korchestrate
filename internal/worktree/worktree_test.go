package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func setupRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-b", "main")
	gitRun(t, dir, "config", "user.name", "t")
	gitRun(t, dir, "config", "user.email", "t@t")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", "init")
	return dir
}

func TestWorktreeLifecycle(t *testing.T) {
	repo := setupRepo(t)

	if clean, err := IsClean(repo); err != nil || !clean {
		t.Fatalf("expected clean repo, clean=%v err=%v", clean, err)
	}
	if !IsRepo(repo) {
		t.Fatal("expected git repo")
	}

	parent := t.TempDir()
	wt := filepath.Join(parent, "wt")
	branch := "test-run"
	if err := Add(repo, wt, branch, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "README.md")); err != nil {
		t.Fatal("worktree missing checked-out files")
	}

	// New file shows up in diff and changed files.
	if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err := Diff(wt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "new.txt") {
		t.Fatalf("diff missing new file:\n%s", diff)
	}
	files, err := ChangedFiles(wt)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "new.txt" {
		t.Fatalf("changed files = %v", files)
	}

	sha, err := Commit(wt, "test commit")
	if err != nil {
		t.Fatal(err)
	}
	if len(sha) < 7 {
		t.Fatalf("bad sha %q", sha)
	}
	if author := strings.TrimSpace(gitRun(t, wt, "log", "-1", "--format=%an <%ae>")); author != "t <t@t>" {
		t.Fatalf("commit author = %q, want the configured identity", author)
	}

	if err := Remove(repo, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still present: %v", err)
	}
	if err := DeleteBranch(repo, branch); err != nil {
		t.Fatal(err)
	}
}

func TestHeadAndClean(t *testing.T) {
	repo := setupRepo(t)
	sha, err := Head(repo)
	if err != nil || len(sha) < 7 {
		t.Fatalf("head=%q err=%v", sha, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if clean, _ := IsClean(repo); clean {
		t.Fatal("expected dirty repo")
	}
}

func TestForBranchAndCheckout(t *testing.T) {
	repo := setupRepo(t)

	parent := t.TempDir()
	wt := filepath.Join(parent, "wt")
	if err := Add(repo, wt, "linked", ""); err != nil {
		t.Fatal(err)
	}
	if got := ForBranch(repo, "linked"); got != wt {
		t.Fatalf("ForBranch(linked) = %q, want %q", got, wt)
	}
	// The main worktree is never returned: runs must not adopt the user's
	// own checkout.
	if got := ForBranch(repo, "main"); got != "" {
		t.Fatalf("ForBranch(main) = %q, want empty", got)
	}
	if got := ForBranch(repo, "missing"); got != "" {
		t.Fatalf("ForBranch(missing) = %q, want empty", got)
	}

	// A branch without a worktree gets one via Checkout.
	gitRun(t, repo, "branch", "plain")
	wt2 := filepath.Join(parent, "wt2")
	if err := Checkout(repo, wt2, "plain"); err != nil {
		t.Fatal(err)
	}
	if got := ForBranch(repo, "plain"); got != wt2 {
		t.Fatalf("ForBranch(plain) = %q, want %q", got, wt2)
	}
}

func TestCurrentBranch(t *testing.T) {
	repo := setupRepo(t)
	branch, err := CurrentBranch(repo)
	if err != nil || branch != "main" {
		t.Fatalf("branch=%q err=%v, want main", branch, err)
	}
	if err := ValidBranch(repo, ""); err == nil {
		t.Fatal("empty branch name should be rejected")
	}
}

func TestCheckoutLock(t *testing.T) {
	repo := setupRepo(t)
	unlock, err := LockCheckout(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if second, err := LockCheckout(repo); err == nil {
		second()
		t.Fatal("concurrent checkout lock succeeded")
	}
	linked := filepath.Join(t.TempDir(), "linked")
	if err := Add(repo, linked, "linked", "HEAD"); err != nil {
		t.Fatal(err)
	}
	other, err := LockCheckout(linked)
	if err != nil {
		t.Fatal("independent worktree was blocked:", err)
	}
	other()
	unlock()
	again, err := LockCheckout(repo)
	if err != nil {
		t.Fatal("lock was not released:", err)
	}
	again()
}

func TestSnapshotIncludesUntrackedWithoutStaging(t *testing.T) {
	repo := setupRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err := Snapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"+changed", "new.txt", "+fresh"} {
		if !strings.Contains(diff, want) {
			t.Errorf("snapshot missing %q:\n%s", want, diff)
		}
	}
	if staged := gitRun(t, repo, "diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "" {
		t.Errorf("snapshot touched the index: %q", staged)
	}
	if st := gitRun(t, repo, "status", "--porcelain"); !strings.Contains(st, "?? new.txt") {
		t.Errorf("new.txt should still be untracked:\n%s", st)
	}
}

func TestList(t *testing.T) {
	repo := setupRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if err := Add(repo, wt, "linked", ""); err != nil {
		t.Fatal(err)
	}
	list, err := List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || !list[0].Main || list[0].Branch != "main" || list[0].Head == "" {
		t.Fatalf("unexpected main worktree: %+v", list)
	}
	if list[1].Main || list[1].Path != wt || list[1].Branch != "linked" {
		t.Fatalf("unexpected linked worktree: %+v", list[1])
	}
}

func TestDefaultAndSeparateBranch(t *testing.T) {
	repo := setupRepo(t)
	if got := DefaultBranch(repo); got != "main" {
		t.Fatalf("DefaultBranch without origin = %q, want main", got)
	}
	// origin/HEAD wins over the local fallback.
	gitRun(t, repo, "branch", "trunk")
	gitRun(t, repo, "update-ref", "refs/remotes/origin/trunk", "HEAD")
	gitRun(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	if got := DefaultBranch(repo); got != "trunk" {
		t.Fatalf("DefaultBranch = %q, want trunk", got)
	}
	for branch, want := range map[string]bool{"trunk": false, "main": true, "feature": true, "": false, "HEAD": false} {
		if got := SeparateBranch(repo, branch); got != want {
			t.Errorf("SeparateBranch(%q) = %v, want %v", branch, got, want)
		}
	}
}
