//go:build windows

package server

import "testing"

func TestDirectoryArchiveWindowsReadsRealAllocationUnit(t *testing.T) {
	free, block, err := archiveDiskSpace(t.TempDir())
	if err != nil || free == 0 || block == 0 || block > 1<<30 {
		t.Fatalf("free=%d block=%d err=%v", free, block, err)
	}
}
