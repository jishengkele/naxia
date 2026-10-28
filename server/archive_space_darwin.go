//go:build darwin

package server

import "golang.org/x/sys/unix"

func archiveDiskSpace(path string) (available, blockSize uint64, err error) {
	var value unix.Statfs_t
	if err = unix.Statfs(path, &value); err != nil {
		return
	}
	blockSize = uint64(value.Bsize)
	available = value.Bavail * blockSize
	return
}
