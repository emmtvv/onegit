package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackAndUnpack(t *testing.T) {
	src := t.TempDir()
	write := func(name, content string, mode os.FileMode) {
		p := filepath.Join(src, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("dist/app", "binary", 0o755)
	write("dist/sub/data.txt", "data", 0o644)
	write("reports/a.xml", "<a/>", 0o644)
	write("reports/b.xml", "<b/>", 0o644)
	write("reports/c.txt", "no", 0o644)
	os.Symlink("app", filepath.Join(src, "dist/link"))

	var buf bytes.Buffer
	var log strings.Builder
	n, err := packPaths(src, []string{"dist", "reports/*.xml", "missing/**", "../outside"}, &buf, &log)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("packed %d files, want 4", n)
	}
	if !strings.Contains(log.String(), "missing/** matches no files") {
		t.Errorf("log = %q", log.String())
	}

	dst := t.TempDir()
	os.WriteFile(filepath.Join(dst, "keep.txt"), []byte("keep"), 0o644)
	if n, err := unpack(dst, bytes.NewReader(buf.Bytes())); err != nil || n != 4 {
		t.Fatalf("unpack: %d %v", n, err)
	}
	for name, want := range map[string]string{"dist/app": "binary", "dist/sub/data.txt": "data", "reports/a.xml": "<a/>",
		"reports/b.xml": "<b/>", "keep.txt": "keep", "dist/link": "binary"} {
		if b, err := os.ReadFile(filepath.Join(dst, name)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v", name, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "reports/c.txt")); err == nil {
		t.Error("reports/c.txt was packed")
	}
	if st, _ := os.Stat(filepath.Join(dst, "dist/app")); st.Mode().Perm()&0o100 == 0 {
		t.Errorf("dist/app lost its executable bit: %v", st.Mode())
	}
	// Restoring again replaces files (caches are restored over checkouts).
	if _, err := unpack(dst, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("second unpack: %v", err)
	}
}

func evilArchive(t *testing.T, entries ...*tar.Header) io.Reader {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		if h.Typeflag == tar.TypeReg {
			h.Size = 1
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestUnpackRefusesEscapes(t *testing.T) {
	for name, entries := range map[string][]*tar.Header{
		"dotdot":         {{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644}},
		"nested dotdot":  {{Name: "a/../../evil", Typeflag: tar.TypeReg, Mode: 0o644}},
		"absolute link":  {{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/etc"}},
		"escaping link":  {{Name: "a/l", Typeflag: tar.TypeSymlink, Linkname: "../../x"}},
		"through a link": {{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "."}, {Name: "l/evil", Typeflag: tar.TypeReg, Mode: 0o644}},
	} {
		ws := t.TempDir()
		_, err := unpack(ws, evilArchive(t, entries...))
		if !errors.Is(err, errUnsafePath) {
			t.Errorf("%s: err = %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(ws), "evil")); err == nil {
			t.Errorf("%s: wrote outside the workspace", name)
		}
	}
	// "/abs" is cleaned into the workspace rather than refused by tar paths;
	// make sure it lands inside.
	ws := t.TempDir()
	if _, err := unpack(ws, evilArchive(t, &tar.Header{Name: "/abs.txt", Typeflag: tar.TypeReg, Mode: 0o644})); err == nil {
		if _, err := os.Stat("/abs.txt"); err == nil {
			t.Error("absolute entry written to /")
		}
	}
}
