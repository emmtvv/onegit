package runner

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// packPaths writes a tar.gz of the given workspace-relative paths (files,
// directories or globs) to w. Paths that match nothing are reported, not
// fatal. It returns the number of files packed.
func packPaths(ws string, patterns []string, w io.Writer, log io.Writer) (int, error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	seen := map[string]bool{}
	files := 0
	add := func(abs string, d fs.DirEntry) error {
		rel, err := filepath.Rel(ws, abs)
		if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
			return nil
		}
		if seen[rel] {
			return nil
		}
		seen[rel] = true
		info, err := os.Lstat(abs)
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(abs); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return nil // sockets, devices: skip
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uname, hdr.Gname, hdr.Uid, hdr.Gid = "", "", 0, 0
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(abs)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }() // read only
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}
		files++
		return nil
	}
	for _, pat := range patterns {
		matches, err := filepath.Glob(filepath.Join(ws, filepath.Clean("/"+pat)))
		if err != nil {
			return files, fmt.Errorf("bad pattern %q: %w", pat, err)
		}
		if len(matches) == 0 {
			fmt.Fprintf(log, "Warning: %s matches no files\n", pat)
			continue
		}
		sort.Strings(matches)
		for _, m := range matches {
			// Parent directories first, so modes and order survive extraction.
			err := filepath.WalkDir(m, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				return add(p, d)
			})
			if err != nil {
				return files, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return files, err
	}
	return files, gz.Close()
}

var errUnsafePath = errors.New("archive entry escapes the workspace")

// unpack extracts a tar.gz into ws. Entries must stay inside ws: absolute
// names, "..", symlinks pointing outside and writes through symlinks are
// refused. Existing files are replaced.
func unpack(ws string, r io.Reader) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return files, err
		}
		target, err := safeTarget(ws, hdr.Name)
		if err != nil {
			return files, fmt.Errorf("%s: %w", hdr.Name, err)
		}
		mode := os.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return files, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			os.Remove(target)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_EXCL, mode|0o200)
			if err != nil {
				return files, err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return files, err
			}
			files++
		case tar.TypeSymlink:
			dest := hdr.Linkname
			if filepath.IsAbs(dest) {
				return files, fmt.Errorf("%s: %w", hdr.Name, errUnsafePath)
			}
			resolved := filepath.Join(filepath.Dir(target), dest)
			if rel, err := filepath.Rel(ws, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return files, fmt.Errorf("%s: %w", hdr.Name, errUnsafePath)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			os.Remove(target)
			if err := os.Symlink(dest, target); err != nil {
				return files, err
			}
		default:
			// Hard links, devices and the like are not restored.
		}
	}
}

// safeTarget maps an entry name into ws and makes sure no existing parent
// directory inside ws is a symlink (which could redirect the write).
func safeTarget(ws, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == "." {
		return "", errUnsafePath
	}
	target := filepath.Join(ws, clean)
	dir := ws
	parts := strings.Split(filepath.Dir(clean), string(filepath.Separator))
	for _, part := range parts {
		if part == "." || part == "" {
			continue
		}
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errUnsafePath
		}
	}
	return target, nil
}
