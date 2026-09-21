//go:build !windows

package migrate

import "syscall"

// freeSpace 返回指定目录所在文件系统的可用字节数。
//
// 只用于「明显放不下时提前拒绝」（规格书 7.4），因此不追求精确：
// 取 Bavail * Bsize 即可，它天然排除了给 root 预留的块，
// 比 Bfree 更贴近「本进程真的能写多少」。
func freeSpace(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
