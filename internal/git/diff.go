package git

import (
	"bufio"
	"context"
	"io"
	"strconv"
	"strings"
)

const EmptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

type LineKind byte

const (
	LineContext LineKind = ' '
	LineAdd     LineKind = '+'
	LineDel     LineKind = '-'
)

type DiffLine struct {
	Kind    LineKind
	OldNo   int // 0 when not applicable
	NewNo   int
	Content string
	NoEOL   bool
}

type Hunk struct {
	Header   string // text after the second @@
	OldStart int
	OldLines int
	NewStart int
	NewLines int
	Lines    []DiffLine
}

type FileStatus string

const (
	StatusAdded    FileStatus = "added"
	StatusDeleted  FileStatus = "deleted"
	StatusModified FileStatus = "modified"
	StatusRenamed  FileStatus = "renamed"
	StatusCopied   FileStatus = "copied"
)

type FileDiff struct {
	OldPath   string
	NewPath   string
	Status    FileStatus
	OldMode   string
	NewMode   string
	Binary    bool
	Additions int
	Deletions int
	Hunks     []*Hunk
	// Truncated is set when the file's hunks were dropped due to size limits.
	Truncated bool
}

// Path returns the most relevant path for display.
func (f *FileDiff) Path() string {
	if f.Status == StatusDeleted {
		return f.OldPath
	}
	return f.NewPath
}

type Diff struct {
	Files     []*FileDiff
	Additions int
	Deletions int
	Truncated bool // some files were omitted or cut short
}

type DiffOptions struct {
	MaxFiles     int // default 500
	MaxLines     int // total lines across files, default 20000
	MaxFileLines int // lines per file, default 3000
	Context      int // context lines, default 3
	Paths        []string
}

// DiffCommit returns the diff introduced by a commit (against its first parent).
func (r *Repo) DiffCommit(ctx context.Context, c *Commit, o DiffOptions) (*Diff, error) {
	base := EmptyTreeSHA
	if len(c.Parents) > 0 {
		base = c.Parents[0]
	}
	return r.DiffRange(ctx, base, c.SHA, o)
}

// DiffRange returns the diff between two tree-ishes.
func (r *Repo) DiffRange(ctx context.Context, from, to string, o DiffOptions) (*Diff, error) {
	if o.MaxFiles == 0 {
		o.MaxFiles = 500
	}
	if o.MaxLines == 0 {
		o.MaxLines = 20000
	}
	if o.MaxFileLines == 0 {
		o.MaxFileLines = 3000
	}
	if o.Context == 0 {
		o.Context = 3
	}
	args := []string{"-c", "core.quotepath=false", "diff", "--no-color", "--no-ext-diff", "-M", "--full-index",
		"-U" + strconv.Itoa(o.Context), from, to, "--"}
	args = append(args, o.Paths...)
	cmd := r.Cmd(ctx, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	d := parseDiff(bufio.NewReaderSize(stdout, 64<<10), o)
	// Drain so git can exit if we stopped early.
	io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	return d, nil
}

func parseDiff(rd *bufio.Reader, o DiffOptions) *Diff {
	d := &Diff{}
	var f *FileDiff
	var h *Hunk
	var oldNo, newNo, totalLines, fileLines int
	for {
		line, err := rd.ReadString('\n')
		if line == "" && err != nil {
			break
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if len(d.Files) >= o.MaxFiles || totalLines >= o.MaxLines {
				d.Truncated = true
				return d
			}
			f = &FileDiff{Status: StatusModified}
			f.OldPath, f.NewPath = splitGitHeader(line[len("diff --git "):])
			d.Files = append(d.Files, f)
			h, fileLines = nil, 0
		case f == nil:
			continue
		case h == nil && strings.HasPrefix(line, "new file mode "):
			f.Status, f.NewMode = StatusAdded, line[len("new file mode "):]
		case h == nil && strings.HasPrefix(line, "deleted file mode "):
			f.Status, f.OldMode = StatusDeleted, line[len("deleted file mode "):]
		case h == nil && strings.HasPrefix(line, "old mode "):
			f.OldMode = line[len("old mode "):]
		case h == nil && strings.HasPrefix(line, "new mode "):
			f.NewMode = line[len("new mode "):]
		case h == nil && strings.HasPrefix(line, "rename from "):
			f.Status, f.OldPath = StatusRenamed, unquote(line[len("rename from "):])
		case h == nil && strings.HasPrefix(line, "rename to "):
			f.NewPath = unquote(line[len("rename to "):])
		case h == nil && strings.HasPrefix(line, "copy from "):
			f.Status, f.OldPath = StatusCopied, unquote(line[len("copy from "):])
		case h == nil && strings.HasPrefix(line, "copy to "):
			f.NewPath = unquote(line[len("copy to "):])
		case h == nil && strings.HasPrefix(line, "Binary files "):
			f.Binary = true
		case h == nil && strings.HasPrefix(line, "--- "):
			if p := line[4:]; p != "/dev/null" {
				f.OldPath = strings.TrimPrefix(unquote(p), "a/")
			}
		case h == nil && strings.HasPrefix(line, "+++ "):
			if p := line[4:]; p != "/dev/null" {
				f.NewPath = strings.TrimPrefix(unquote(p), "b/")
			}
		case strings.HasPrefix(line, "@@ "):
			h = parseHunkHeader(line)
			oldNo, newNo = h.OldStart, h.NewStart
			if !f.Truncated {
				f.Hunks = append(f.Hunks, h)
			}
		case h != nil && len(line) > 0 && line[0] == '\\':
			if n := len(h.Lines); n > 0 {
				h.Lines[n-1].NoEOL = true
			}
		case h != nil:
			kind := LineContext
			content := line
			if len(line) > 0 {
				kind, content = LineKind(line[0]), line[1:]
			}
			dl := DiffLine{Kind: kind, Content: content}
			switch kind {
			case LineAdd:
				dl.NewNo = newNo
				newNo++
				f.Additions++
				d.Additions++
			case LineDel:
				dl.OldNo = oldNo
				oldNo++
				f.Deletions++
				d.Deletions++
			default:
				dl.Kind = LineContext
				dl.OldNo, dl.NewNo = oldNo, newNo
				oldNo++
				newNo++
			}
			fileLines++
			totalLines++
			if fileLines > o.MaxFileLines && !f.Truncated {
				f.Truncated, f.Hunks = true, nil
				d.Truncated = true
			}
			if !f.Truncated {
				h.Lines = append(h.Lines, dl)
			}
		}
	}
	return d
}

func parseHunkHeader(line string) *Hunk {
	h := &Hunk{}
	// @@ -a,b +c,d @@ header
	rest := strings.TrimPrefix(line, "@@ ")
	ranges, header, _ := strings.Cut(rest, " @@")
	h.Header = strings.TrimSpace(header)
	for _, part := range strings.Fields(ranges) {
		start, count := parseRange(part[1:])
		if part[0] == '-' {
			h.OldStart, h.OldLines = start, count
		} else if part[0] == '+' {
			h.NewStart, h.NewLines = start, count
		}
	}
	return h
}

func parseRange(s string) (start, count int) {
	a, b, ok := strings.Cut(s, ",")
	start, _ = strconv.Atoi(a)
	count = 1
	if ok {
		count, _ = strconv.Atoi(b)
	}
	return
}

// splitGitHeader handles "a/x b/y" (paths without spaces are unambiguous;
// others are fixed up later by ---/+++ or rename lines).
func splitGitHeader(s string) (string, string) {
	if strings.HasPrefix(s, `"`) {
		return "", ""
	}
	if i := strings.Index(s, " b/"); i > 0 {
		return strings.TrimPrefix(s[:i], "a/"), s[i+3:]
	}
	return s, s
}

func unquote(s string) string {
	s = strings.TrimSuffix(s, "\t")
	if strings.HasPrefix(s, `"`) {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
	}
	return s
}
