//go:build !windows

package util

// HideConsoleWindow is a no-op on non-Windows platforms.
func HideConsoleWindow() {}
