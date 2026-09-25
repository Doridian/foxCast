//go:build !linux

package sender

import "time"

// Non-Linux platforms retain the process-relative monotonic fallback.
func bootRelativeNow() time.Duration {
	return time.Since(appStartTime)
}
