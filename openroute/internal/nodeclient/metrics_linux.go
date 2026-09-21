//go:build linux

package nodeclient

import "syscall"

func diskUsage() (total, used int64) {
	var stat syscall.Statfs_t
	if syscall.Statfs("/", &stat) != nil {
		return 0, 0
	}
	return int64(stat.Blocks) * stat.Bsize, int64(stat.Blocks-stat.Bfree) * stat.Bsize
}
