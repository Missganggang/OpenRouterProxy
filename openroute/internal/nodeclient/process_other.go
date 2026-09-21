//go:build !linux

package nodeclient

import (
	"context"
	"errors"
	"time"
)

func executeCommand(context.Context, string, time.Duration) (string, error) {
	return "", errors.New("remote command execution requires Linux")
}
func startPTY(context.Context, int, int) (ptySession, error) {
	return nil, errors.New("interactive terminals require Linux")
}
