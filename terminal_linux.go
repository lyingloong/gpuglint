package main

import (
	"os"
	"syscall"
	"unsafe"
)

func terminalMode(f *os.File) (bool, func()) {
	var term syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&term)))
	return e == 0 && os.Getenv("TERM") != "dumb", func() {}
}
