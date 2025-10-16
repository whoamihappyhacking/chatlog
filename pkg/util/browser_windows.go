//go:build windows

package util

import (
	"errors"
	"os/exec"
	"syscall"
)

func browserCommand(url string) (*exec.Cmd, error) {
	if url == "" {
		return nil, errors.New("empty url")
	}
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd, nil
}
