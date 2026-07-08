package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fakeZipFiles makes a []*zip.File-like slice we can feed to detectWrapperDir.
// We can't easily construct zip.File literals because the struct uses unexported
// fields, so the helper writes a real zip into a temp file and returns a reader.
// The wrapper-detect tests below use that real-zip path.

func writeTestZip(t *testing.T, entries map[string]string) string {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "test.zip")
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	// Deterministic order so test failures are readable.
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, name := range keys {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create entry %s: %v", name, err)
		}
		if _, err := w.Write([]byte(entries[name])); err != nil {
			t.Fatalf("write entry %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return tmp
}

func openZipFiles(t *testing.T, path string) []*zip.File {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	t.Cleanup(func() { zr.Close() })
	return zr.File
}

func TestDetectWrapperDir(t *testing.T) {
	cases := []struct {
		name    string
		entries map[string]string
		want    string
	}{
		{
			name: "single wrapper dir",
			entries: map[string]string{
				"project/index.html":      "x",
				"project/css/style.css":   "y",
				"project/js/main.js":      "z",
			},
			want: "project/",
		},
		{
			name: "file at zip root → no wrapper",
			entries: map[string]string{
				"index.html":    "x",
				"css/style.css": "y",
			},
			want: "",
		},
		{
			name: "multiple top-level dirs → no wrapper",
			entries: map[string]string{
				"a/x.html": "x",
				"b/y.html": "y",
			},
			want: "",
		},
		{
			name: "wrapper plus __MACOSX junk → wrapper detected",
			entries: map[string]string{
				"project/index.html":            "x",
				"project/css/style.css":         "y",
				"__MACOSX/project/._index.html": "junk",
				"__MACOSX/._project":            "junk",
			},
			want: "project/",
		},
		{
			name: "wrapper plus .DS_Store junk → wrapper detected",
			entries: map[string]string{
				"site/index.html":   "x",
				"site/.DS_Store":    "junk",
				".DS_Store":         "junk-at-root",
			},
			want: "site/",
		},
		{
			name:    "empty zip → empty wrapper",
			entries: map[string]string{},
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTestZip(t, tc.entries)
			files := openZipFiles(t, path)
			got := detectWrapperDir(files)
			if got != tc.want {
				t.Fatalf("detectWrapperDir = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsZipJunk(t *testing.T) {
	junk := []string{
		"__MACOSX/foo",
		"__MACOSX/foo/._bar",
		".DS_Store",
		"some/dir/.DS_Store",
		"Thumbs.db",
		"some/dir/Thumbs.db",
		"desktop.ini",
	}
	for _, name := range junk {
		if !isZipJunk(name) {
			t.Errorf("isZipJunk(%q) = false, want true", name)
		}
	}
	ok := []string{
		"index.html",
		"project/index.html",
		"project/.htaccess",
		"some/.gitkeep",
		"some/.well-known/acme-challenge",
		"MACOSX-not-the-real-one.html",
	}
	for _, name := range ok {
		if isZipJunk(name) {
			t.Errorf("isZipJunk(%q) = true, want false", name)
		}
	}
}

func TestZipToTar_StripsWrapper(t *testing.T) {
	zipPath := writeTestZip(t, map[string]string{
		"clinisresearchprototype/index.html":            "<html>hi</html>",
		"clinisresearchprototype/css/style.css":         "body{}",
		"clinisresearchprototype/js/main.js":            "var x=1;",
		"clinisresearchprototype/.DS_Store":             "junk",
		"__MACOSX/clinisresearchprototype/._index.html": "junk",
	})

	r, err := zipToTar(zipPath, "clinis")
	if err != nil {
		t.Fatalf("zipToTar: %v", err)
	}

	got := readTarNames(t, r)
	want := []string{
		"clinis/css/style.css",
		"clinis/index.html",
		"clinis/js/main.js",
	}
	sort.Strings(got)
	if !equalStringSlices(got, want) {
		t.Fatalf("tar entries:\n got:  %v\n want: %v", got, want)
	}
}

func TestZipToTar_PreservesStructureWhenNoWrapper(t *testing.T) {
	zipPath := writeTestZip(t, map[string]string{
		"index.html":      "<html>hi</html>",
		"css/style.css":   "body{}",
		"about/index.html": "<html>about</html>",
	})

	r, err := zipToTar(zipPath, "mysite")
	if err != nil {
		t.Fatalf("zipToTar: %v", err)
	}

	got := readTarNames(t, r)
	want := []string{
		"mysite/about/index.html",
		"mysite/css/style.css",
		"mysite/index.html",
	}
	sort.Strings(got)
	if !equalStringSlices(got, want) {
		t.Fatalf("tar entries:\n got:  %v\n want: %v", got, want)
	}
}

func TestZipToTar_DropsJunkEverywhere(t *testing.T) {
	zipPath := writeTestZip(t, map[string]string{
		"index.html":               "x",
		".DS_Store":                "junk",
		"a/Thumbs.db":              "junk",
		"a/index.html":             "y",
		"__MACOSX/_._index.html":   "junk",
	})

	r, err := zipToTar(zipPath, "s")
	if err != nil {
		t.Fatalf("zipToTar: %v", err)
	}

	got := readTarNames(t, r)
	for _, n := range got {
		if strings.Contains(n, "__MACOSX") || strings.Contains(n, ".DS_Store") || strings.Contains(n, "Thumbs.db") {
			t.Errorf("junk leaked into tar: %s", n)
		}
	}
}

func readTarNames(t *testing.T, r io.Reader) []string {
	t.Helper()
	tr := tar.NewReader(r)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		names = append(names, hdr.Name)
		// Consume body to advance.
		if _, err := io.Copy(io.Discard, tr); err != nil {
			t.Fatalf("tar copy: %v", err)
		}
	}
	return names
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// touch keeps the import of "bytes" used if compiler complains in future refactors.
var _ = bytes.NewBuffer
