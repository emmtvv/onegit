package git

import (
	"bytes"
	"context"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

type EntryType string

const (
	EntryBlob      EntryType = "blob"
	EntryTree      EntryType = "tree"
	EntrySubmodule EntryType = "commit"
)

type TreeEntry struct {
	Name string
	Path string
	Type EntryType
	Mode string
	SHA  string
	Size int64
}

func (e TreeEntry) IsDir() bool     { return e.Type == EntryTree }
func (e TreeEntry) IsSymlink() bool { return e.Mode == "120000" }

// ObjectType returns "blob", "tree" or "" if the path does not exist.
func (r *Repo) ObjectType(ctx context.Context, commit, p string) string {
	if p == "" {
		return "tree"
	}
	out, err := r.run(ctx, nil, "cat-file", "-t", commit+":"+p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ListTree lists a directory at a commit, directories first.
func (r *Repo) ListTree(ctx context.Context, commit, dir string) ([]TreeEntry, error) {
	spec := commit
	if dir != "" {
		spec = commit + ":" + dir
	}
	out, err := r.run(ctx, nil, "ls-tree", "-z", "--long", spec)
	if err != nil {
		return nil, ErrNotExist
	}
	var entries []TreeEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		// <mode> SP <type> SP <sha> SP+ <size> TAB <name>
		meta, name, ok := strings.Cut(string(rec), "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 4 {
			continue
		}
		size, _ := strconv.ParseInt(f[3], 10, 64)
		entries = append(entries, TreeEntry{
			Name: name, Path: path.Join(dir, name), Mode: f[0], Type: EntryType(f[1]), SHA: f[2], Size: size,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

// BlobSize returns the size of a file at a commit.
func (r *Repo) BlobSize(ctx context.Context, commit, p string) (int64, error) {
	out, err := r.run(ctx, nil, "cat-file", "-s", commit+":"+p)
	if err != nil {
		return 0, ErrNotExist
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// ReadBlob reads up to limit bytes of a file (limit <= 0 means everything).
func (r *Repo) ReadBlob(ctx context.Context, commit, p string, limit int64) ([]byte, error) {
	cmd := r.Cmd(ctx, "cat-file", "blob", commit+":"+p)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var rd io.Reader = stdout
	if limit > 0 {
		rd = io.LimitReader(stdout, limit)
	}
	b, err := io.ReadAll(rd)
	if limit > 0 {
		io.Copy(io.Discard, stdout)
	}
	if werr := cmd.Wait(); werr != nil && err == nil {
		return nil, ErrNotExist
	}
	return b, err
}

// StreamBlob writes the whole blob to w.
func (r *Repo) StreamBlob(ctx context.Context, commit, p string, w io.Writer) error {
	cmd := r.Cmd(ctx, "cat-file", "blob", commit+":"+p)
	cmd.Stdout = w
	return cmd.Run()
}

// IsBinary uses git's heuristic: a NUL byte in the first 8000 bytes.
func IsBinary(b []byte) bool {
	if len(b) > 8000 {
		b = b[:8000]
	}
	return bytes.IndexByte(b, 0) >= 0
}

// MatchDirs expands a pattern with one "*" segment ("services/*" or
// "projects/*/project.yaml") to the names of the directories at the "*"
// that contain the rest of the pattern.
func (r *Repo) MatchDirs(ctx context.Context, commit, pattern string) []string {
	prefix, rest, ok := strings.Cut(pattern, "*")
	if !ok {
		return nil
	}
	entries, err := r.ListTree(ctx, commit, strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name)
		}
	}
	if rest == "" || rest == "/" || len(out) == 0 {
		return out
	}
	// Keep the directories that have the rest of the pattern, checking all
	// of them in one git process.
	var in strings.Builder
	for _, name := range out {
		in.WriteString(commit + ":" + prefix + name + rest + "\n")
	}
	res, err := r.run(ctx, strings.NewReader(in.String()), "cat-file", "--batch-check=%(objecttype)")
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(res), "\n"), "\n")
	var found []string
	for i, name := range out {
		if i < len(lines) && !strings.HasSuffix(lines[i], " missing") && !strings.HasSuffix(lines[i], " ambiguous") {
			found = append(found, name)
		}
	}
	return found
}
