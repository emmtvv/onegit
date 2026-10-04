package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
)

// GrepOptions controls a content search.
type GrepOptions struct {
	Pattern    string
	Regex      bool     // extended regular expression; fixed string otherwise
	IgnoreCase bool     //
	Paths      []string // pathspecs, e.g. "services/api", ":(glob)**/*.go", ":(exclude)vendor"
	MaxMatches int      // default 1000
	MaxLineLen int      // longer lines are cut; default 400
}

type GrepMatch struct {
	Path string
	Line int
	Text string
}

// ErrBadPattern is returned when git rejects the search pattern.
var ErrBadPattern = errors.New("invalid search pattern")

// Grep searches the files of a commit. It stops after MaxMatches matches and
// reports whether results were cut short. Binary files are skipped.
func (r *Repo) Grep(ctx context.Context, commit string, o GrepOptions) ([]GrepMatch, bool, error) {
	if o.MaxMatches <= 0 {
		o.MaxMatches = 1000
	}
	if o.MaxLineLen <= 0 {
		o.MaxLineLen = 400
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"-c", "core.quotepath=false", "-c", "grep.threads=0", "grep", "-n", "-z", "-I", "--full-name"}
	if o.Regex {
		args = append(args, "-E")
	} else {
		args = append(args, "-F")
	}
	if o.IgnoreCase {
		args = append(args, "-i")
	}
	args = append(args, "-e", o.Pattern, commit, "--")
	args = append(args, o.Paths...)
	cmd := r.Cmd(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	prefix := commit + ":"
	var res []GrepMatch
	truncated := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		// <commit>:<path> NUL <line> NUL <text>
		f := strings.SplitN(sc.Text(), "\x00", 3)
		if len(f) != 3 {
			continue
		}
		n, _ := strconv.Atoi(f[1])
		text := f[2]
		if len(text) > o.MaxLineLen {
			text = cutUTF8(text, o.MaxLineLen)
		}
		res = append(res, GrepMatch{Path: strings.TrimPrefix(f[0], prefix), Line: n, Text: text})
		if len(res) >= o.MaxMatches {
			truncated = true
			break
		}
	}
	if truncated || sc.Err() != nil {
		cancel()
	}
	werr := cmd.Wait()
	var ee interface{ ExitCode() int }
	switch {
	case truncated:
		return res, true, nil
	case ctx.Err() != nil:
		return res, true, ctx.Err()
	case werr == nil:
		return res, false, sc.Err()
	case errors.As(werr, &ee) && ee.ExitCode() == 1:
		return res, false, nil // no matches
	}
	if msg := stderr.String(); strings.Contains(msg, "regex") || strings.Contains(msg, "-e option") || strings.Contains(msg, "Invalid") || strings.Contains(msg, "Unmatched") {
		return nil, false, ErrBadPattern
	}
	return nil, false, &RunError{Args: []string{"grep", o.Pattern}, Err: werr, Stderr: strings.TrimSpace(stderr.String())}
}

// FindFiles lists the paths in a commit for which match returns true, up to
// limit; truncated reports that more would have matched.
func (r *Repo) FindFiles(ctx context.Context, commit string, limit int, match func(path string) bool) ([]string, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := r.Cmd(ctx, "ls-tree", "-r", "-z", "--name-only", "--end-of-options", commit)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	var res []string
	truncated := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, 0); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		p := sc.Text()
		if !match(p) {
			continue
		}
		if len(res) >= limit {
			truncated = true
			break
		}
		res = append(res, p)
	}
	if truncated {
		cancel() // stop git early; its exit status no longer matters
		_ = cmd.Wait()
		return res, true, nil
	}
	if err := cmd.Wait(); err != nil {
		return nil, false, err
	}
	return res, false, sc.Err()
}

func cutUTF8(s string, n int) string {
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
