package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Checkout struct {
	Path   string `json:"path"`
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
}

func git(path string, args ...string) (string, error) {
	return command(nil, "git", append([]string{"-C", path}, args...)...)
}

func checkout(path string) (Checkout, error) {
	root, err := git(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return Checkout{}, err
	}
	list, err := git(root, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return Checkout{}, err
	}
	first, _, _ := strings.Cut(list, "\x00")
	repo, ok := strings.CutPrefix(first, "worktree ")
	if !ok {
		return Checkout{}, fmt.Errorf("unexpected Git worktree record")
	}
	branch, err := git(root, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		branch, err = git(root, "rev-parse", "--short", "HEAD")
	}
	return Checkout{root, repo, branch}, err
}

func createTree(base, path, name string) (Checkout, error) {
	info, err := checkout(path)
	if err != nil {
		return Checkout{}, err
	}
	ref, err := git(path, "check-ref-format", "--branch", name)
	if err != nil || ref != name {
		return Checkout{}, fmt.Errorf("invalid branch name %q", name)
	}
	hash := sha256.Sum256([]byte(info.Repo))
	parent := filepath.Join(base, "worktrees", fmt.Sprintf("%s-%x", filepath.Base(info.Repo), hash[:6]))
	target := filepath.Join(parent, name)
	if _, err := os.Lstat(target); err == nil {
		c, err := checkout(target)
		if err == nil && c.Path == target && c.Repo == info.Repo && c.Branch == name {
			return c, nil
		}
		return Checkout{}, fmt.Errorf("worktree path already exists: %s", target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return Checkout{}, err
	}
	refs, err := git(path, "for-each-ref", "--format=%(refname)", "refs/heads/"+name)
	if err != nil {
		return Checkout{}, err
	}
	args := []string{"worktree", "add", "-b", name, "--", target}
	for _, r := range strings.Split(refs, "\n") {
		if r == "refs/heads/"+name {
			args = []string{"worktree", "add", "--", target, name}
			break
		}
	}
	if _, err := git(path, args...); err != nil {
		return Checkout{}, err
	}
	return checkout(target)
}
