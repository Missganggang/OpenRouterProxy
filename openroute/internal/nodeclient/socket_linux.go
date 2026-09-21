//go:build linux

package nodeclient

import (
	"golang.org/x/sys/unix"
	"net"
	"os"
	"strconv"
	"syscall"
)

func configureSocketMark(d *net.Dialer) error {
	value := os.Getenv("OUTBOUND_FWMARK")
	if value == "" {
		return nil
	}
	mark, err := strconv.ParseUint(value, 0, 32)
	if err != nil {
		return err
	}
	d.Control = func(network, address string, c syscall.RawConn) error {
		var markErr error
		err := c.Control(func(fd uintptr) { markErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(mark)) })
		if err != nil {
			return err
		}
		return markErr
	}
	return nil
}
