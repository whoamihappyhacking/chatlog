package tray

// Options controls how the tray icon behaves.
type Options struct {
	Tooltip string
	OnOpen  func()
	OnQuit  func()
}

// Controller allows callers to stop the tray icon when shutting down.
type Controller interface {
	Stop()
}
