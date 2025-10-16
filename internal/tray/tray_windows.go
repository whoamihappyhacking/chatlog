//go:build windows

package tray

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/getlantern/systray"
	"github.com/rs/zerolog/log"
)

type controller struct {
	stopOnce sync.Once
	stopped  chan struct{}
}

func newController() *controller {
	return &controller{stopped: make(chan struct{})}
}

func (c *controller) Stop() {
	c.stopOnce.Do(func() {
		systray.Quit()
	})
	<-c.stopped
}

var (
	iconInit sync.Once
	iconData []byte
	iconErr  error
)

func trayIcon() ([]byte, error) {
	iconInit.Do(func() {
		iconData, iconErr = loadIcon()
	})
	return iconData, iconErr
}

func loadIcon() ([]byte, error) {
	const iconName = "icon.ico"

	paths := []string{
		iconName,
	}

	exePath, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exePath)
		paths = append(paths, filepath.Join(exeDir, iconName))
	}

	var errs error
	for _, candidate := range paths {
		if data, readErr := os.ReadFile(candidate); readErr == nil {
			return data, nil
		} else {
			errs = errors.Join(errs, readErr)
		}
	}

	return nil, errs
}

// Start launches the Windows notification area icon.
func Start(opts Options) (Controller, error) {
	ctrl := newController()
	ready := make(chan struct{})

	go systray.Run(func() {
		setupTray(opts, ctrl)
		close(ready)
	}, func() {
		close(ctrl.stopped)
	})

	<-ready
	return ctrl, nil
}

func setupTray(opts Options, ctrl *controller) {
	if data, err := trayIcon(); err != nil {
		log.Warn().Err(err).Msg("failed to load tray icon from icon.ico")
	} else if len(data) > 0 {
		systray.SetIcon(data)
	} else {
		log.Warn().Msg("tray icon icon.ico is empty")
	}

	tip := opts.Tooltip
	if tip == "" {
		tip = "Chatlog"
	}
	systray.SetTooltip(tip)

	openItem := systray.AddMenuItem("Open Chatlog", "Open Chatlog web interface")
	systray.AddSeparator()
	quitItem := systray.AddMenuItem("Exit Chatlog", "Quit Chatlog")

	go func() {
		for {
			select {
			case <-openItem.ClickedCh:
				if opts.OnOpen != nil {
					opts.OnOpen()
				}
			case <-quitItem.ClickedCh:
				if opts.OnQuit != nil {
					opts.OnQuit()
				}
				ctrl.Stop()
				return
			case <-ctrl.stopped:
				return
			}
		}
	}()
}
