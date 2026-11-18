// Package state publishes an ephemeral snapshot owned by one monitor process.
package state

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrInactive = errors.New("no live gpuglint snapshot")

const marker = "gpuglint state lock v1\n"

// Lock offsets lie beyond the marker so Windows' mandatory byte locks do
// not interfere with reading it. The ready lock is taken only after the
// first publication; crash leftovers are never visible to Read on restart.
const (
	ownerByte = 4096
	readyByte = 4097
)

type Writer struct {
	mu         sync.Mutex
	path       string
	lock       *os.File
	createdDir bool
	published  os.FileInfo
	ready      bool
}

func retrySharing(op func() error) error {
	deadline := time.Now().Add(time.Second)
	for {
		err := op()
		if !sharingConflict(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func remove(path string) error {
	err := retrySharing(func() error { return unlinkFile(path) })
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Use an open handle for file identity on Windows as well as Linux.
func fileInfo(path string) (os.FileInfo, error) {
	f, err := openFile(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

func removeEmptyDir(dir string) error {
	err := os.Remove(dir)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if entries, listErr := os.ReadDir(dir); listErr == nil && len(entries) > 0 {
		return nil
	}
	return err
}

// Open reserves path. The caller supplies the initial snapshot through Write.
func Open(path string) (*Writer, error) {
	if path == "" {
		return nil, errors.New("empty state path")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return nil, err
	}
	// A competing writer can remove its empty directory between mkdir
	// and open. Retry against the current path when that happens.
	for attempts := 0; attempts < 8; attempts++ {
		err := os.Mkdir(dir, 0700)
		createdDir := err == nil
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		w, err := acquire(path, createdDir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil && createdDir {
			_ = removeEmptyDir(dir)
		}
		return w, err
	}
	return nil, fmt.Errorf("state path changed repeatedly: %s", path)
}

func acquire(path string, createdDir bool) (*Writer, error) {
	lockPath := path + ".lock"
	f, err := openFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_EXCL)
	created := err == nil
	if errors.Is(err, os.ErrExist) {
		f, err = openFile(lockPath, os.O_RDWR)
	}
	if err != nil {
		return nil, err
	}
	retain := false
	defer func() {
		if !retain {
			f.Close()
		}
	}()
	locked, err := tryLock(f, ownerByte, true)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, fmt.Errorf("another monitor owns %s", path)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("invalid lock file: %s", lockPath)
	}
	current, err := fileInfo(lockPath)
	if err != nil {
		return nil, err
	}
	// Another owner could have unlinked this inode after our open and
	// before our lock. Never publish under a detached lock.
	if !os.SameFile(info, current) {
		return nil, os.ErrNotExist
	}
	defer func() {
		if created && !retain {
			_ = remove(lockPath)
		}
	}()
	content, err := io.ReadAll(io.LimitReader(f, 128))
	if err != nil {
		return nil, err
	}
	recovering := string(content) == marker || string(content) == marker+"directory\n"
	if !recovering && len(content) != 0 {
		return nil, fmt.Errorf("unrecognized lock file: %s", lockPath)
	}
	for _, reserved := range []string{path, path + ".tmp"} {
		entry, err := os.Lstat(reserved)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !recovering || !entry.Mode().IsRegular() {
			return nil, fmt.Errorf("refusing to replace an existing file: %s", reserved)
		}
		if err := remove(reserved); err != nil {
			return nil, err
		}
	}
	createdDir = createdDir || strings.HasSuffix(string(content), "directory\n")
	if !recovering {
		record := marker
		if createdDir {
			record += "directory\n"
		}
		if _, err := f.WriteString(record); err != nil {
			return nil, err
		}
	}
	retain = true
	return &Writer{path: path, lock: f, createdDir: createdDir}, nil
}

// Write atomically replaces the snapshot with exactly data.
func (w *Writer) Write(data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lock == nil {
		return os.ErrClosed
	}
	tmp := w.path + ".tmp"
	f, err := openFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = remove(tmp)
		}
	}()
	_, writeErr := f.Write(data)
	info, statErr := f.Stat()
	if err := errors.Join(writeErr, statErr, f.Close()); err != nil {
		return err
	}
	if err := retrySharing(func() error { return replaceFile(tmp, w.path) }); err != nil {
		return err
	}
	published = true
	w.published = info
	if !w.ready {
		// An inactive-reader probe may briefly hold a shared ready lock.
		deadline := time.Now().Add(time.Second)
		for {
			locked, err := tryLock(w.lock, readyByte, true)
			if err != nil || locked {
				w.ready = locked
				return err
			}
			if time.Now().After(deadline) {
				return errors.New("a reader is holding the snapshot readiness lock")
			}
			time.Sleep(time.Millisecond)
		}
	}
	return nil
}

// Read checks the writer's readiness lock before opening the snapshot.
func Read(path string) ([]byte, error) {
	lock, err := openFile(path+".lock", os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrInactive
	}
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	inactive, err := tryLock(lock, readyByte, false)
	if err != nil {
		return nil, err
	}
	if inactive {
		return nil, ErrInactive
	}
	f, err := openFile(path, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrInactive
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// Close removes the published file while holding the ownership lock.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lock == nil {
		return nil
	}
	var err error
	if w.published != nil {
		current, statErr := fileInfo(w.path)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
		case statErr != nil:
			err = statErr
		case os.SameFile(w.published, current):
			err = remove(w.path)
		}
	}
	// Keep the recovery marker if the state file couldn't be removed.
	if err == nil {
		err = remove(w.lock.Name())
	}
	err = errors.Join(err, w.lock.Close())
	w.lock = nil
	if w.createdDir {
		err = errors.Join(err, removeEmptyDir(filepath.Dir(w.path)))
	}
	return err
}
