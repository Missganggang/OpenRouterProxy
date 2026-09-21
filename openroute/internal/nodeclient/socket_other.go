//go:build !linux

package nodeclient

import (
	"errors"
	"net"
	"os"
)

func configureSocketMark(d *net.Dialer) error {
	if os.Getenv("OUTBOUND_FWMARK") != "" {
		return errors.New("OUTBOUND_FWMARK is supported only on Linux")
	}
	return nil
}
