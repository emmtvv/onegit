package git

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrRefChanged is returned by UpdateRef when the ref no longer points at the
// expected old value (someone else updated it concurrently).
var ErrRefChanged = errors.New("ref was updated concurrently")

// IsAncestor reports whether commit a is an ancestor of (or equal to) b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	err := r.Cmd(ctx, "merge-base", "--is-ancestor", a, b).Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// MergeResult is the outcome of a server-side three-way merge.
type MergeResult struct {
	Tree      string   // resulting tree (with conflict markers when conflicted)
	Conflicts []string // paths with conflicts; empty means a clean merge
}

// MergeTree merges theirs into ours without a worktree. mergeBase may be
// empty to let git compute it.
func (r *Repo) MergeTree(ctx context.Context, mergeBase, ours, theirs string) (*MergeResult, error) {
	args := []string{"-c", "core.quotepath=false", "merge-tree", "--write-tree", "--name-only", "--no-messages"}
	if mergeBase != "" {
		args = append(args, "--merge-base", mergeBase)
	}
	args = append(args, ours, theirs)
	cmd := r.Cmd(ctx, args...)
	out, err := cmd.Output()
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && ee.ExitCode() == 1) {
		stderr := ""
		if ee != nil {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, &RunError{Args: args, Err: err, Stderr: stderr}
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if !isHex(lines[0]) || len(lines[0]) < 40 {
		// Exit code 1 also means "not something we can merge" (bad revision).
		return nil, &RunError{Args: args, Err: err, Stderr: stderrOf(err)}
	}
	res := &MergeResult{Tree: lines[0]}
	if err != nil { // exit code 1: conflicts, one path per line
		for _, l := range lines[1:] {
			if l == "" {
				break
			}
			res.Conflicts = append(res.Conflicts, unquote(l))
		}
		if len(res.Conflicts) == 0 {
			res.Conflicts = []string{"(unknown)"}
		}
	}
	return res, nil
}

// CommitTree creates a commit object and returns its SHA.
func (r *Repo) CommitTree(ctx context.Context, tree string, parents []string, message string, author, committer Signature) (string, error) {
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	cmd := r.Cmd(ctx, args...)
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_NAME="+author.Name, "GIT_AUTHOR_EMAIL="+author.Email, "GIT_AUTHOR_DATE="+gitDate(author.When),
		"GIT_COMMITTER_NAME="+committer.Name, "GIT_COMMITTER_EMAIL="+committer.Email, "GIT_COMMITTER_DATE="+gitDate(committer.When),
	)
	cmd.Stdin = strings.NewReader(message)
	out, err := cmd.Output()
	if err != nil {
		return "", &RunError{Args: args, Err: err, Stderr: stderrOf(err)}
	}
	return strings.TrimSpace(string(out)), nil
}

func gitDate(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return strconv.FormatInt(t.Unix(), 10) + " " + t.Format("-0700")
}

func stderrOf(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return ""
}

// UpdateRef atomically moves ref from oldSHA to newSHA. Use ZeroSHA as oldSHA
// to require that the ref does not exist yet, or "" to skip the check.
// newSHA == ZeroSHA deletes the ref.
func (r *Repo) UpdateRef(ctx context.Context, ref, newSHA, oldSHA string) error {
	var args []string
	if newSHA == ZeroSHA {
		args = []string{"update-ref", "-d", ref}
	} else {
		args = []string{"update-ref", ref, newSHA}
	}
	if oldSHA != "" {
		args = append(args, oldSHA)
	}
	cmd := r.Cmd(ctx, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := string(out)
		if strings.Contains(msg, "but expected") || strings.Contains(msg, "is at") || strings.Contains(msg, "reference already exists") {
			return ErrRefChanged
		}
		return &RunError{Args: args, Err: err, Stderr: strings.TrimSpace(msg)}
	}
	return nil
}

// ReplayCommit is a commit with the full raw message, for rebasing.
type ReplayCommit struct {
	SHA     string
	Parent  string
	Author  Signature
	Message string
}

// CommitsToReplay lists non-merge commits in from..to, oldest first.
func (r *Repo) CommitsToReplay(ctx context.Context, from, to string) ([]ReplayCommit, error) {
	cmd := r.Cmd(ctx, "log", "--reverse", "--no-merges", "--format=%H%x00%P%x00%an%x00%ae%x00%aI%x00%B%x1e", from+".."+to)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var res []ReplayCommit
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := strings.IndexByte(string(data), 0x1e); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		f := strings.SplitN(strings.TrimLeft(sc.Text(), "\n"), "\x00", 6)
		if len(f) != 6 {
			continue
		}
		// Strict ISO keeps the author's original time zone offset.
		at, _ := time.Parse(time.RFC3339, f[4])
		parent, _, _ := strings.Cut(f[1], " ")
		res = append(res, ReplayCommit{
			SHA: f[0], Parent: parent,
			Author:  Signature{Name: f[2], Email: f[3], When: at},
			Message: f[5],
		})
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	return res, sc.Err()
}

// BlobIDs resolves "<rev>:<path>" specs to object IDs; missing ones map to "".
func (r *Repo) BlobIDs(ctx context.Context, specs []string) map[string]string {
	res := make(map[string]string, len(specs))
	if len(specs) == 0 {
		return res
	}
	cmd := r.Cmd(ctx, "cat-file", "--batch-check=%(objectname)")
	cmd.Stdin = strings.NewReader(strings.Join(specs, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return res
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for i, spec := range specs {
		if i < len(lines) && !strings.HasSuffix(lines[i], " missing") {
			res[spec] = lines[i]
		}
	}
	return res
}

// ChangedFiles lists paths changed between two commits; a rename is listed
// as both its old and new path.
func (r *Repo) ChangedFiles(ctx context.Context, from, to string) ([]string, error) {
	return r.diffNames(ctx, from, to, "--no-renames")
}

// CountChangedFiles counts changed files the way a diff view shows them
// (renames count once).
func (r *Repo) CountChangedFiles(ctx context.Context, from, to string) (int, error) {
	files, err := r.diffNames(ctx, from, to, "-M")
	return len(files), err
}

func (r *Repo) diffNames(ctx context.Context, from, to, renames string) ([]string, error) {
	out, err := r.run(ctx, nil, "diff", "--name-only", renames, "-z", from, to, "--")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// CountCommits counts commits in from..to.
func (r *Repo) CountCommits(ctx context.Context, from, to string) (int, error) {
	out, err := r.run(ctx, nil, "rev-list", "--count", from+".."+to)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}
