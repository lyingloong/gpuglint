package state

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var lockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
var setFileInformationByHandle = syscall.NewLazyDLL("kernel32.dll").NewProc("SetFileInformationByHandle")

// Use POSIX rename/unlink semantics on local filesystems that support them.
// MoveFileEx(REPLACE_EXISTING) can deny even an open FILE_SHARE_DELETE
// reader. A POSIX rename preserves its handle to the previous snapshot.
func replaceFile(src, dst string) error {
	const fileRenameInfoEx = 22
	const replaceIfExists = 1
	const posixSemantics = 2
	type renameInfo struct {
		Flags          uint32
		RootDirectory  uintptr
		FileNameLength uint32
		FileName       [1]uint16
	}
	name, err := syscall.UTF16FromString(dst)
	if err != nil {
		return err
	}
	// FILE_RENAME_INFO ends in an inline, variable-length UTF-16 name.
	var header renameInfo
	buf := make([]byte, int(unsafe.Sizeof(header))+len(name)*2)
	info := (*renameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = replaceIfExists | posixSemantics
	info.FileNameLength = uint32((len(name) - 1) * 2)
	copy(unsafe.Slice(&info.FileName[0], len(name)), name)
	err = changeFileInfo(src, fileRenameInfoEx, buf)
	if unsupportedInfo(err) {
		return os.Rename(src, dst)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}

func unlinkFile(path string) error {
	const fileDispositionInfoEx = 21
	const dispositionDelete = 1
	const posixSemantics = 2
	err := changeFileInfo(path, fileDispositionInfoEx, []byte{dispositionDelete | posixSemantics, 0, 0, 0})
	if unsupportedInfo(err) {
		return os.Remove(path)
	}
	if err != nil {
		return &os.PathError{Op: "remove", Path: path, Err: err}
	}
	return nil
}

func changeFileInfo(path string, class uintptr, buf []byte) error {
	const deleteAccess = 0x00010000
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := syscall.CreateFile(name, deleteAccess, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	ok, _, callErr := setFileInformationByHandle.Call(uintptr(h), class, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	runtime.KeepAlive(buf)
	if ok == 0 {
		return callErr
	}
	return nil
}

func unsupportedInfo(err error) bool {
	// ERROR_INVALID_FUNCTION, ERROR_NOT_SUPPORTED, ERROR_INVALID_PARAMETER.
	return errors.Is(err, syscall.Errno(1)) || errors.Is(err, syscall.Errno(50)) || errors.Is(err, syscall.Errno(87))
}

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
