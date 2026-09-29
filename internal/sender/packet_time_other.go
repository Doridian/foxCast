//go:build !linux

package sender

import (
	"errors"
	"net"
	"time"
)

func enablePacketTimestamps(*net.UDPConn) error {
	return errors.New("kernel receive timestamps are only used on Linux")
}

func packetTime(_ []byte, readAt time.Time) time.Time {
	return readAt
}
