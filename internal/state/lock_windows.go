package state

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var lockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

// Share delete access so another process can replace a snapshot while
// this handle is reading its previous contents.
func openFile(path string, flags int) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(syscall.GENERIC_READ)
	if flags&os.O_RDWR != 0 {
		access |= syscall.GENERIC_WRITE
	}
	mode := uint32(syscall.OPEN_EXISTING)
	if flags&os.O_CREATE != 0 {
		mode = syscall.CREATE_NEW
	}
	h, err := syscall.CreateFile(name, access, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, mode, syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

func tryLock(f *os.File, offset int64, exclusive bool) (bool, error) {
	const failImmediately = 1
	const exclusiveLock = 2
	flags := uintptr(failImmediately)
	if exclusive {
		flags |= exclusiveLock
	}
	pos := syscall.Overlapped{Offset: uint32(offset)}
	ok, _, err := lockFileEx.Call(f.Fd(), flags, 0, 1, 0, uintptr(unsafe.Pointer(&pos)))
	runtime.KeepAlive(f)
	if ok != 0 {
		return true, nil
	}
	if errors.Is(err, syscall.Errno(33)) { // ERROR_LOCK_VIOLATION
		return false, nil
	}
	return false, err
}

func sharingConflict(err error) bool {
	// Some clients open files without FILE_SHARE_DELETE. Give a short
	// reader time to finish before reporting a failed replace or cleanup.
	return errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}
