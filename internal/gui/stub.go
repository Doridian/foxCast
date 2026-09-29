//go:build nogui

package gui

import "context"

// Run reports ErrUnavailable: this build has no GUI.
func Run(context.Context, Backend) error {
	return ErrUnavailable
}
