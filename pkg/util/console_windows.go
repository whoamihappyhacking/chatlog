//go:build windows

package util

import "syscall"

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procFreeConsole = kernel32.NewProc("FreeConsole")
)

// HideConsoleWindow detaches the process from the current console so no window is shown.
func HideConsoleWindow() {
	procFreeConsole.Call()
}
