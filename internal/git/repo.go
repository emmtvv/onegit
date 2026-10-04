// Package git wraps the git CLI for all repository operations. Shelling out
// to git keeps behaviour identical to what clients expect and stays fast on
// large monorepos.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var ErrNotExist = errors.New("object does not exist")

const ZeroSHA = "0000000000000000000000000000000000000000"

type Repo struct {
	Path string
	env  []string
}

// WithEnv returns a copy of the repo handle whose git commands get extra
// environment, e.g. the quarantine object directories during pre-receive.
func (r *Repo) WithEnv(env []string) *Repo {
	return &Repo{Path: r.Path, env: append(append([]string(nil), r.env...), env...)}
}

// Open initialises the bare repository if needed and (re)installs hooks that
// call back into the given onegit binary.
func Open(ctx context.Context, path, defaultBranch, selfExe string) (*Repo, error) {
	r := &Repo{Path: path}
	if _, err := os.Stat(filepath.Join(path, "HEAD")); os.IsNotExist(err) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, err
		}
		if _, err := r.run(ctx, nil, "init", "--bare", "--initial-branch="+defaultBranch); err != nil {
			return nil, err
		}
	}
	for k, v := range map[string]string{
		"http.receivepack":              "true",
		"receive.advertisePushOptions":  "true",
		"uploadpack.allowFilter":        "true",
		"uploadpack.allowAnySHA1InWant": "true",
		"core.logAllRefUpdates":         "true",
		// Housekeeping runs in the background (Maintain), never inside a push.
		"gc.auto":             "0",
		"receive.autogc":      "false",
		"core.commitGraph":    "true",
		"gc.writeCommitGraph": "false", // Maintain writes it, with Bloom filters
		"repack.writeBitmaps": "true",
	} {
		if _, err := r.run(ctx, nil, "config", k, v); err != nil {
			return nil, err
		}
	}
	return r, r.installHooks(selfExe)
}

func (r *Repo) installHooks(selfExe string) error {
	dir := filepath.Join(r.Path, "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"pre-receive", "post-receive"} {
		script := fmt.Sprintf("#!/bin/sh\n# managed by onegit, do not edit\nexec %q hook %s\n", selfExe, name)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// Cmd builds a git command bound to the repository.
func (r *Repo) Cmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", r.Path}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, r.env...)
	return cmd
}

func (r *Repo) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := r.Cmd(ctx, args...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, &RunError{Args: args, Err: err, Stderr: strings.TrimSpace(errb.String())}
	}
	return out.Bytes(), nil
}

type RunError struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *RunError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
}

func (e *RunError) Unwrap() error { return e.Err }

// IsEmpty reports whether the repository has no refs yet.
func (r *Repo) IsEmpty(ctx context.Context) bool {
	out, err := r.run(ctx, nil, "for-each-ref", "--count=1", "--format=%(refname)")
	return err == nil && len(bytes.TrimSpace(out)) == 0
}

// SetHead points HEAD at the given branch.
func (r *Repo) SetHead(ctx context.Context, branch string) error {
	_, err := r.run(ctx, nil, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	return err
}

// HeadBranch returns the branch HEAD points to.
func (r *Repo) HeadBranch(ctx context.Context) string {
	out, err := r.run(ctx, nil, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
