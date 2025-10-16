//go:build !windows

package util

import (
	"errors"
	"os/exec"
	"runtime"
)

func browserCommand(url string) (*exec.Cmd, error) {
	if url == "" {
		return nil, errors.New("empty url")
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url), nil
	default:
		return exec.Command("xdg-open", url), nil
	}
}
