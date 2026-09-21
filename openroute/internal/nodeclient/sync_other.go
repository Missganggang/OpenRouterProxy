//go:build !linux

package nodeclient

func syncDirectory(string) error { return nil }
