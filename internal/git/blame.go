package git

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"time"
)

// BlameHunk is a run of consecutive lines last changed by the same commit.
type BlameHunk struct {
	Commit    *Commit
	StartLine int // first line number in the blamed file (1-based)
	Lines     []string
	// Previous is where these lines were before the commit changed them
	// (empty when the commit added them), for "blame before this change".
	PrevSHA, PrevPath string
}

// Blame attributes every line of path at commit to the commit that last
// changed it. Consecutive lines from the same commit form one hunk.
// commit must be a resolved commit id (blame misparses --end-of-options).
func (r *Repo) Blame(ctx context.Context, commit, path string) ([]*BlameHunk, error) {
	if !isHex(commit) {
		return nil, ErrNotExist
	}
	cmd := r.Cmd(ctx, "blame", "--porcelain", commit, "--", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	hunks, perr := parseBlame(stdout)
	if err := cmd.Wait(); err != nil {
		return nil, &RunError{Args: []string{"blame", path}, Err: err, Stderr: strings.TrimSpace(stderr.String())}
	}
	return hunks, perr
}

// parseBlame reads `git blame --porcelain`: per line a header
// "<sha> <orig-line> <final-line> [<group-size>]", the commit's metadata the
// first time the commit appears, then the content prefixed with a tab.
func parseBlame(rd io.Reader) ([]*BlameHunk, error) {
	commits := map[string]*Commit{}
	prev := map[*Commit][2]string{}
	var hunks []*BlameHunk
	var cur *Commit
	var final int
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if content, ok := strings.CutPrefix(line, "\t"); ok {
			if cur == nil {
				continue
			}
			if n := len(hunks); n > 0 && hunks[n-1].Commit == cur && hunks[n-1].StartLine+len(hunks[n-1].Lines) == final {
				hunks[n-1].Lines = append(hunks[n-1].Lines, content)
			} else {
				p := prev[cur]
				hunks = append(hunks, &BlameHunk{Commit: cur, StartLine: final, Lines: []string{content}, PrevSHA: p[0], PrevPath: p[1]})
			}
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		if len(key) == 40 && isHex(key) {
			f := strings.Fields(val)
			if len(f) >= 2 {
				final, _ = strconv.Atoi(f[1])
			}
			c, ok := commits[key]
			if !ok {
				c = &Commit{SHA: key}
				commits[key] = c
			}
			cur = c
			continue
		}
		if cur == nil {
			continue
		}
		switch key {
		case "author":
			cur.Author.Name = val
		case "author-mail":
			cur.Author.Email = strings.Trim(val, "<>")
		case "author-time":
			ts, _ := strconv.ParseInt(val, 10, 64)
			cur.Author.When = time.Unix(ts, 0)
		case "committer":
			cur.Committer.Name = val
		case "committer-mail":
			cur.Committer.Email = strings.Trim(val, "<>")
		case "committer-time":
			ts, _ := strconv.ParseInt(val, 10, 64)
			cur.Committer.When = time.Unix(ts, 0)
		case "summary":
			cur.Subject = val
		case "previous":
			if sha, file, ok := strings.Cut(val, " "); ok {
				prev[cur] = [2]string{sha, unquote(file)}
			}
		}
	}
	return hunks, sc.Err()
}
