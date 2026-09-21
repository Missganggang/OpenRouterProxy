//go:build windows

package migrate

import (
	"syscall"
	"unsafe"
)

// freeSpace 返回指定目录所在卷的可用字节数（Windows 实现）。
//
// 用 GetDiskFreeSpaceEx 而不是遍历目录：它直接给出「调用者可用的空闲字节数」，
// 已经扣除了配额限制，正是磁盘空间检查需要的语义。
func freeSpace(dir string) (int64, error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetDiskFreeSpaceExW")

	dirPtr, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}

	var freeToCaller, total, totalFree uint64
	ret, _, callErr := proc.Call(
		uintptr(unsafe.Pointer(dirPtr)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if ret == 0 {
		// callErr 在成功时是 "The operation completed successfully"，
		// 只在 ret == 0 时才有意义。
		return 0, callErr
	}
	return int64(freeToCaller), nil
}
