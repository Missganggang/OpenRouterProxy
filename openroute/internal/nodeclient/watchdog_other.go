//go:build !linux

package nodeclient

import (
	"context"
	"errors"
)

func (*Client) armUpgradeWatchdog(context.Context, string, string, string) (func(), error) {
	return nil, errors.New("upgrade recovery requires Linux")
}
func (*Client) confirmUpgrade() error { return nil }
func RunUpgradeWatchdog(string) error { return errors.New("upgrade recovery requires Linux") }
