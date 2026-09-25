//go:build linux

package sender

import (
	"time"

	"golang.org/x/sys/unix"
)

// bootRelativeNow returns the system monotonic boot clock, including suspend.
// AirPlay timing packets and frame timestamps use this domain on Apple senders.
func bootRelativeNow() time.Duration {
	var timestamp unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &timestamp); err == nil {
		return time.Duration(timestamp.Sec)*time.Second + time.Duration(timestamp.Nsec)
	}
	return time.Since(appStartTime)
}
