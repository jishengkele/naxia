package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDirectoryArchiveSealAndVerify(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "database"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "database", "rows.json"), []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	hash, _, err := sealDirectoryArchive(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyDirectoryArchive(root, hash); err != nil {
		raw, _ := os.ReadFile(filepath.Join(root, archiveInventoryName))
		t.Log(string(raw))
		actual, _ := archiveInventory(root, true)
		b, _ := json.Marshal(actual)
		t.Log(string(b))
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "database", "rows.json"), []byte("[1]"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyDirectoryArchive(root, hash); err == nil {
		t.Fatal("modified file accepted")
	}
}

func TestDirectoryArchiveDetectsMissingExtraAndInventoryTampering(t *testing.T) {
	for _, mutation := range []string{"missing", "extra", "inventory", "marker"} {
		t.Run(mutation, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "data"), []byte{0, 255, 1}, 0600); err != nil {
				t.Fatal(err)
			}
			hash, _, err := sealDirectoryArchive(root)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "missing":
				err = os.Remove(filepath.Join(root, "data"))
			case "extra":
				err = os.WriteFile(filepath.Join(root, "unexpected"), nil, 0600)
			case "inventory":
				err = os.WriteFile(filepath.Join(root, archiveInventoryName), []byte("{}\n"), 0600)
			case "marker":
				err = os.WriteFile(filepath.Join(root, archiveCompleteName), []byte("wrong"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = verifyDirectoryArchive(root, hash); err == nil {
				t.Fatal("corrupt archive accepted")
			}
		})
	}
}

func TestDirectoryArchiveSymlinksEmptyDirectoriesAndBinary(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte{0, 255, 10, 0, 128}
	if err := os.WriteFile(filepath.Join(source, "binary"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("outside-does-not-exist", filepath.Join(source, "link")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	entries, err := archiveInventory(source, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = copyArchiveTree(t.Context(), source, target, entries); err != nil {
		t.Fatal(err)
	}
	copied, err := archiveInventory(target, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = sameArchiveInventory(entries, copied, true); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(filepath.Join(target, "link"))
	if err != nil || link != "outside-does-not-exist" {
		t.Fatalf("link=%q err=%v", link, err)
	}
	checksum, _, err := sealDirectoryArchive(target)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyDirectoryArchive(target, checksum); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryArchiveCapacityMathAndSafePaths(t *testing.T) {
	required, err := archiveRequiredSpace(1024, 100, 4096)
	if err != nil || required <= 1024+64*1024*1024 {
		t.Fatalf("required=%d err=%v", required, err)
	}
	if _, err = archiveRequiredSpace(^uint64(0), 1, 4096); err == nil {
		t.Fatal("size overflow accepted")
	}
	if _, err = archiveRequiredSpace(1, 1000001, 4096); err == nil {
		t.Fatal("unbounded inventory accepted")
	}
	for _, name := range []string{"../escape", "/absolute", "a/../../escape"} {
		if _, err = archiveRelativePath(name); err == nil {
			t.Fatalf("unsafe path accepted: %s", name)
		}
	}
	root := t.TempDir()
	t.Setenv("ARTEX_ARCHIVE_DIR", root)
	if err = removeManagedArchive(t.TempDir(), root); err == nil {
		t.Fatal("archive root deletion allowed")
	}
	if err = removeManagedArchive(t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("unrelated directory deletion allowed")
	}
}

func TestDirectoryArchiveRejectsDestinationInsideSourceViaSymlink(t *testing.T) {
	source := t.TempDir()
	link := filepath.Join(t.TempDir(), "archive-link")
	if err := os.Symlink(source, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := validateArchiveSourceSeparation(source, filepath.Join(link, "not-created", "tasks")); err == nil {
		t.Fatal("recursive archive destination accepted")
	}
	if err := validateArchiveSourceSeparation(source, filepath.Join(t.TempDir(), "external")); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryArchiveRejectsGrowth(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	file := filepath.Join(source, "live.log")
	if err := os.WriteFile(file, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := archiveInventory(source, false)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("after")
	f.Close()
	if err := copyArchiveTree(context.Background(), source, target, entries); err == nil {
		t.Fatal("growing file accepted")
	}
}
