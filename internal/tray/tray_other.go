//go:build !windows

package tray

type noopController struct{}

func (noopController) Stop() {}

// Start is a no-op on platforms without a system tray implementation.
func Start(opts Options) (Controller, error) {
	return noopController{}, nil
}
