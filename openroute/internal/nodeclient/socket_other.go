//go:build !linux

package nodeclient

import (
	"errors"
	"net"
)

func validateSocketPolicy(p dialPolicy) error {
	if p.Interface != "" {
		return errors.New("TUNNEL_INTERFACE is supported only on Linux")
	}
	if p.MarkSet {
		return errors.New("OUTBOUND_FWMARK and TUNNEL_FWMARK are supported only on Linux")
	}
	return nil
}
func configureSocketPolicy(d *net.Dialer, p dialPolicy) error { return validateSocketPolicy(p) }
