//go:build windows

package server

import (
	"errors"
	"golang.org/x/sys/windows"
	"path/filepath"
	"unsafe"
)

var archiveGetDiskFreeSpace = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetDiskFreeSpaceW")

func archiveDiskSpace(path string) (available, blockSize uint64, err error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return 0, 0, err
	}
	name, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return 0, 0, err
	}
	err = windows.GetDiskFreeSpaceEx(name, &available, nil, nil)
	if err != nil {
		return 0, 0, err
	}
	var volume [32768]uint16
	if err = windows.GetVolumePathName(name, &volume[0], uint32(len(volume))); err != nil {
		return 0, 0, err
	}
	var sectors, bytesPerSector, freeClusters, totalClusters uint32
	ok, _, callErr := archiveGetDiskFreeSpace.Call(uintptr(unsafe.Pointer(&volume[0])), uintptr(unsafe.Pointer(&sectors)), uintptr(unsafe.Pointer(&bytesPerSector)), uintptr(unsafe.Pointer(&freeClusters)), uintptr(unsafe.Pointer(&totalClusters)))
	if ok == 0 {
		return 0, 0, callErr
	}
	blockSize = uint64(sectors) * uint64(bytesPerSector)
	if blockSize == 0 {
		return 0, 0, errors.New("文件系统分配单元大小无效")
	}
	return available, blockSize, nil
}
