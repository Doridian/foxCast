package sender

import (
	"log"
	"sync/atomic"
)

var debugMode atomic.Bool

// SetDebugMode controls verbose AirPlay logging. It is safe to call while
// capture or receiver workers are active.
func SetDebugMode(enabled bool) {
	debugMode.Store(enabled)
}

// DebugMode reports whether verbose AirPlay logging is enabled.
func DebugMode() bool {
	return debugMode.Load()
}

// dbg logs a message only when debug mode is enabled.
func dbg(format string, args ...interface{}) {
	if DebugMode() {
		log.Printf(format, args...)
	}
}
