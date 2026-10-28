//go:build linux

package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectoryArchiveLinuxOpenWriterBlocksEvenWithoutAppending(t *testing.T) {
	root := t.TempDir()
	f, err := os.OpenFile(filepath.Join(root, "background.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = archiveCheckWriters([]string{root}); err == nil || !strings.Contains(err.Error(), "PID") {
		t.Fatalf("idle writable descriptor accepted: %v", err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = archiveCheckWriters([]string{root}); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(filepath.Join(root, "background.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err = archiveCheckWriters([]string{root}); err != nil {
		t.Fatalf("read-only descriptor rejected: %v", err)
	}
}
