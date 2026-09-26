// Package worktree manages the isolated git worktree each pipeline run uses, so
// the target repository's working tree is never touched.
package worktree

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out.String(), nil
}

// IsRepo reports whether dir is inside a git work tree.
func IsRepo(dir string) bool {
	out, err := git(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// Head returns the current commit SHA of repo.
func Head(repo string) (string, error) {
	out, err := git(repo, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// IsClean reports whether the repo has no staged, unstaged or untracked changes.
func IsClean(repo string) (bool, error) {
	out, err := git(repo, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// LockCheckout prevents two kor runs from editing or publishing one checkout.
// The OS releases the lock even if a process exits unexpectedly.
func LockCheckout(repo string) (func(), error) {
	dir, err := git(repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(strings.TrimSpace(dir), "kor-run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("checkout is busy with another kor run: %w", err)
	}
	return func() { _ = f.Close() }, nil
}

// Add creates a new worktree at path on a fresh branch.
func Add(repo, path, branch, base string) error {
	if base == "" {
		base = "HEAD"
	}
	_, err := git(repo, "worktree", "add", "-b", branch, path, base)
	return err
}

// Checkout creates a worktree at path on the existing branch.
func Checkout(repo, path, branch string) error {
	_, err := git(repo, "worktree", "add", path, branch)
	return err
}

// Info describes one worktree registered with the repository.
type Info struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"` // empty when HEAD is detached
	Head   string `json:"head"`
	Main   bool   `json:"main,omitempty"` // the repository's own checkout
	// Prunable worktrees are registered but their directory is gone.
	Prunable bool `json:"prunable,omitempty"`
}

// List returns every worktree of repo, the main checkout first.
func List(repo string) ([]Info, error) {
	out, err := git(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var list []Info
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "worktree "):
			list = append(list, Info{Path: strings.TrimSpace(strings.TrimPrefix(line, "worktree")), Main: len(list) == 0})
		case len(list) == 0:
		case strings.HasPrefix(line, "HEAD "):
			list[len(list)-1].Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			list[len(list)-1].Branch = strings.TrimPrefix(line, "branch refs/heads/")
		case line == "prunable" || strings.HasPrefix(line, "prunable "):
			list[len(list)-1].Prunable = true
		}
	}
	return list, nil
}

// ForBranch returns the path of the linked worktree that has branch checked
// out, or "" when the branch is not checked out in any linked worktree. The
// main worktree (the repository itself) never counts: runs must not adopt the
// user's own checkout.
func ForBranch(repo, branch string) string {
	list, _ := List(repo)
	for _, wt := range list {
		if !wt.Main && wt.Branch == branch {
			return wt.Path
		}
	}
	return ""
}

// Prune drops worktree registrations whose directories no longer exist.
func Prune(repo string) error {
	_, err := git(repo, "worktree", "prune")
	return err
}

// CurrentBranch returns the branch checked out in repo. It returns "HEAD" when
// the repo is in a detached-HEAD state.
func CurrentBranch(repo string) (string, error) {
	out, err := git(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ValidBranch reports whether name can be used as a new branch name. An empty
// name is rejected; callers that allow a derived default handle that case
// before calling this.
func ValidBranch(repo, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("a worktree name is required")
	}
	if _, err := git(repo, "check-ref-format", "--branch", name); err != nil {
		return fmt.Errorf("invalid worktree name %q", name)
	}
	return nil
}

// BranchExists reports whether a local branch already exists.
func BranchExists(repo, branch string) bool {
	_, err := git(repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// Remove deletes a worktree, force-removing it if needed.
func Remove(repo, path string) error {
	if _, err := git(repo, "worktree", "remove", "--force", path); err != nil {
		return err
	}
	_, _ = git(repo, "worktree", "prune")
	return nil
}

// DeleteBranch removes a run branch after its worktree is gone.
func DeleteBranch(repo, branch string) error {
	_, err := git(repo, "branch", "-D", branch)
	return err
}

// ChangedFiles lists files reported as modified/added/deleted in the worktree.
func ChangedFiles(worktree string) ([]string, error) {
	out, err := git(worktree, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if i := strings.LastIndex(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		files = append(files, strings.Trim(path, `"`))
	}
	return files, nil
}

// Diff returns the full diff of the worktree including untracked files.
func Diff(worktree string) (string, error) {
	if _, err := git(worktree, "add", "-A", "-N"); err != nil {
		return "", err
	}
	out, err := git(worktree, "diff", "HEAD", "--no-color", "--no-ext-diff")
	if err != nil {
		return "", err
	}
	return out, nil
}

// Snapshot returns the worktree's changes against HEAD, including untracked
// files, without touching the index. It is safe to call while an agent is
// working in the worktree, unlike Diff which marks files intent-to-add.
func Snapshot(worktree string) (string, error) {
	out, err := git(worktree, "diff", "HEAD", "--no-color", "--no-ext-diff")
	if err != nil {
		return "", err
	}
	others, err := git(worktree, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return out, err
	}
	var b strings.Builder
	b.WriteString(out)
	for _, f := range strings.Split(others, "\x00") {
		if f == "" {
			continue
		}
		// --no-index exits 1 when the files differ, which is always the case
		// here, so the output is kept regardless of the error.
		d, _ := git(worktree, "diff", "--no-color", "--no-ext-diff", "--no-index", "--", "/dev/null", f)
		b.WriteString(d)
	}
	return b.String(), nil
}

// Apply applies a patch file (a Diff of this branch) to the worktree and
// stages it.
func Apply(worktree, patch string) error {
	_, err := git(worktree, "apply", "--index", "--whitespace=nowarn", patch)
	return err
}

// Stage adds every change in the worktree to the index without committing.
func Stage(worktree string) error {
	_, err := git(worktree, "add", "-A")
	return err
}

// Push publishes branch to origin and sets its upstream.
func Push(worktree, branch string) error {
	_, err := git(worktree, "push", "-u", "origin", branch)
	return err
}

// Commit stages everything and creates a commit, returning the new SHA. It is a
// no-op when there is nothing to commit.
func Commit(worktree, message string) (string, error) {
	status, err := git(worktree, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) == "" {
		return "", nil
	}
	if err := checkIdentity(worktree); err != nil {
		return "", err
	}
	if _, err := git(worktree, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := git(worktree, "commit", "-m", message); err != nil {
		return "", err
	}
	out, err := git(worktree, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// checkIdentity ensures commits are authored by the user's own git identity
// (repo or global config, or GIT_AUTHOR_EMAIL), so GitHub links them to the
// user's account.
func checkIdentity(worktree string) error {
	if os.Getenv("GIT_AUTHOR_EMAIL") != "" {
		return nil
	}
	out, _ := git(worktree, "config", "user.email")
	if strings.TrimSpace(out) != "" {
		return nil
	}
	return errors.New(`git identity not configured; run: git config --global user.name "Your Name" && git config --global user.email "you@example.com" (use an email linked to your GitHub account)`)
}
