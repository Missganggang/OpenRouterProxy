//go:build linux

package nodeclient

import (
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"syscall"
)

func applySocketPolicy(fd int, p dialPolicy) error {
	if p.Interface != "" {
		if err := unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, p.Interface); err != nil {
			return fmt.Errorf("bind tunnel socket to interface %s: %w", p.Interface, err)
		}
	}
	if p.MarkSet {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_MARK, int(p.Mark)); err != nil {
			return fmt.Errorf("set socket mark %d: %w", p.Mark, err)
		}
	}
	return nil
}
func validateSocketPolicy(p dialPolicy) error {
	if p.Interface == "" && !p.MarkSet {
		return nil
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return applySocketPolicy(fd, p)
}
func configureSocketPolicy(d *net.Dialer, p dialPolicy) error {
	if p.Interface == "" && !p.MarkSet {
		return nil
	}
	if p.Interface != "" {
		if _, err := net.InterfaceByName(p.Interface); err != nil {
			return fmt.Errorf("tunnel interface %s unavailable: %w", p.Interface, err)
		}
	}
	d.Control = func(network, address string, c syscall.RawConn) error {
		var policyErr error
		err := c.Control(func(fd uintptr) { policyErr = applySocketPolicy(int(fd), p) })
		if err != nil {
			return err
		}
		return policyErr
	}
	return nil
}
