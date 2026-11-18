package main

import (
	"os"
	"syscall"
	"unsafe"
)

func terminalSize(f *os.File) (width, height int) {
	var size struct {
		Rows, Cols, Xpixel, Ypixel uint16
	}
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&size)))
	if err != 0 || size.Cols == 0 || size.Rows == 0 {
		return 0, 0
	}
	return int(size.Cols), int(size.Rows)
}

func terminalMode(f *os.File) (bool, func()) {
	var term syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&term)))
	return e == 0 && os.Getenv("TERM") != "dumb", func() {}
}
