package main

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	setConsoleMode             = kernel32.NewProc("SetConsoleMode")
	getConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

func terminalSize(f *os.File) (width, height int) {
	// srWindow is an inclusive rectangle inside the larger screen buffer.
	var info struct {
		Size, CursorPosition [2]int16
		Attributes           uint16
		Window               struct{ Left, Top, Right, Bottom int16 }
		MaximumWindowSize    [2]int16
	}
	ok, _, _ := getConsoleScreenBufferInfo.Call(f.Fd(), uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		return 0, 0
	}
	return int(info.Window.Right) - int(info.Window.Left) + 1, int(info.Window.Bottom) - int(info.Window.Top) + 1
}

func terminalMode(f *os.File) (bool, func()) {
	var mode uint32
	if syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) != nil {
		return false, func() {}
	}
	const enableVirtualTerminalProcessing = 0x0004
	r, _, _ := setConsoleMode.Call(f.Fd(), uintptr(mode|enableVirtualTerminalProcessing))
	if r == 0 {
		return false, func() {}
	}
	return true, func() { setConsoleMode.Call(f.Fd(), uintptr(mode)) }
}
