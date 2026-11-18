package state

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceAndCloseWithOpenWindowsReaders(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".gpuglint")
	path := filepath.Join(dir, "state")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	hold := func(path string) *os.File {
		t.Helper()
		f, err := openFile(path, os.O_RDONLY)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	old := hold(path)
	hold(path + ".lock")
	if err := w.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(old); err != nil || string(got) != "first" {
		t.Fatalf("replaced snapshot handle: %q, %v", got, err)
	}
	current := hold(path)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(current); err != nil || string(got) != "second" {
		t.Fatalf("last snapshot handle: %q, %v", got, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state directory remained visible to new readers: %v", err)
	}
}
