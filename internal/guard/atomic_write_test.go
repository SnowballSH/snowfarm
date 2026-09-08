package guard

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAtomicInstallsTheFileWithItsMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "marker")

	if err := writeAtomic(path, []byte("first\n"), fs.FileMode(0o640)); err != nil {
		t.Fatalf("write: %v", err)
	}

	content, err := os.ReadFile(path) // #nosec G304 -- the path is the test's own temporary directory
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != "first\n" {
		t.Fatalf("the file holds %q, want %q", content, "first\n")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("the file is %v, want 0640", info.Mode().Perm())
	}
	assertNoLeftovers(t, dir)
}

func TestWriteAtomicReplacesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "marker")
	if err := os.WriteFile(path, []byte("stale\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := writeAtomic(path, []byte("fresh\n"), fs.FileMode(0o640)); err != nil {
		t.Fatalf("write: %v", err)
	}

	content, err := os.ReadFile(path) // #nosec G304 -- the path is the test's own temporary directory
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != "fresh\n" {
		t.Fatalf("the file holds %q, want %q", content, "fresh\n")
	}
	assertNoLeftovers(t, dir)
}

func assertNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 1 || names[0] != "marker" {
		t.Fatalf("the directory holds %v, want only the installed file", names)
	}
}
