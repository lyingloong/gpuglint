package main

import (
	"os"
	"syscall"
)

var setConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")

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
