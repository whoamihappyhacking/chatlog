package util

import "fmt"

// OpenBrowser launches the user's default browser with the provided URL.
func OpenBrowser(url string) error {
	cmd, err := browserCommand(url)
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser: %w", err)
	}
	return nil
}
