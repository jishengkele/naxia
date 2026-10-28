//go:build !linux && !windows && !darwin

package server

import "errors"

func archiveDiskSpace(path string) (uint64, uint64, error) {
	return 0, 0, errors.New("当前平台不支持可靠读取归档剩余空间")
}
