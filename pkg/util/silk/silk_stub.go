//go:build !cgo
// +build !cgo

package silk

import "fmt"

// Silk2MP3 is a no-op stub when built without CGO; returns an error so callers can fall back.
func Silk2MP3(data []byte) ([]byte, error) {
	return nil, fmt.Errorf("silk2mp3 unavailable: built without cgo")
}
