package state

import (
	"errors"
	"os"
	"syscall"
)

func openFile(path string, flags int) (*os.File, error) {
	return os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
}

// Open-file-description locks are released with this descriptor, even if
// another goroutine opens and closes the same inode.
func tryLock(f *os.File, offset int64, exclusive bool) (bool, error) {
	const fOFDSetLK = 37
	lock := syscall.Flock_t{Type: syscall.F_RDLCK, Start: offset, Len: 1}
	if exclusive {
		lock.Type = syscall.F_WRLCK
	}
	err := syscall.FcntlFlock(f.Fd(), fOFDSetLK, &lock)
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func sharingConflict(error) bool { return false }

func replaceFile(src, dst string) error { return os.Rename(src, dst) }
func unlinkFile(path string) error      { return os.Remove(path) }
