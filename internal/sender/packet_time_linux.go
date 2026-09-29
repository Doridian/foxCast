//go:build linux

package sender

import (
	"net"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// enablePacketTimestamps asks the kernel to stamp each received datagram
// (SO_TIMESTAMPNS), so PTP arrival times leave out scheduling delay.
func enablePacketTimestamps(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1)
	}); err != nil {
		return err
	}
	return sockErr
}

// packetTime is a datagram's kernel receive time from its control messages,
// on the local monotonic clock of readAt (the time the read returned), or
// readAt when the kernel gave none.
func packetTime(oob []byte, readAt time.Time) time.Time {
	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return readAt
	}
	for _, m := range messages {
		var ts unix.Timespec
		size := int(unsafe.Sizeof(ts))
		if m.Header.Level != unix.SOL_SOCKET || m.Header.Type != unix.SCM_TIMESTAMPNS || len(m.Data) < size {
			continue
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(&ts)), size), m.Data)
		// The kernel stamps with CLOCK_REALTIME; carry the offset from the
		// read's wall time onto its monotonic reading.
		return readAt.Add(time.Duration(ts.Nano() - readAt.UnixNano()))
	}
	return readAt
}
