//go:build !linux

package nodeclient

// Linux is the deployment target; other platforms remain buildable for development.
func diskUsage() (total, used int64) { return 0, 0 }
