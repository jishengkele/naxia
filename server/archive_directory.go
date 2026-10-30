package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/taskproc"
)

const directoryArchiveKind = "directory"
const archiveInventoryName = "inventory.ndjson"
const archiveCompleteName = "COMPLETE"

func syncArchiveDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	} // Files are individually flushed; Windows cannot fsync directory handles.
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncArchiveAncestors(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for current := absolute; ; current = filepath.Dir(current) {
		if err := syncArchiveDirectory(current); err != nil {
			return err
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

func writeArchiveFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, archiveFileMode)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, f.Sync(), f.Close())
}

type archiveFileRecord struct {
	Path     string    `json:"path"`
	Kind     string    `json:"kind"`
	Size     int64     `json:"size"`
	Mode     uint32    `json:"mode"`
	Modified time.Time `json:"modified"`
	Link     string    `json:"link,omitempty"`
	SHA256   string    `json:"sha256,omitempty"`
}

type archiveSource struct {
	Source   string              `json:"source"`
	Relative string              `json:"relative"`
	Entries  []archiveFileRecord `json:"entries"`
	Owned    bool                `json:"owned"`
}

type archiveSpaceEstimate struct {
	EstimatedBytes      uint64             `json:"estimated_bytes"`
	RequiredBytes       uint64             `json:"required_bytes"`
	AvailableBytes      uint64             `json:"available_bytes"`
	FileCount           int64              `json:"file_count"`
	Sufficient          bool               `json:"sufficient"`
	SnapshotAt          time.Time          `json:"snapshot_at"`
	Writers             []archiveWriter    `json:"writers,omitempty"`
	BackgroundProcesses []taskproc.Process `json:"background_processes,omitempty"`
}

func archiveInventory(root string, hash bool) ([]archiveFileRecord, error) {
	var records []archiveFileRecord
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if len(records) >= maxArchiveFiles {
			return errors.New("归档文件条目超过安全上限，原数据保留")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("读取归档文件属性 %s: %w", path, err)
		}
		r := archiveFileRecord{Path: filepath.ToSlash(relative), Size: info.Size(), Mode: uint32(info.Mode().Perm()), Modified: info.ModTime()}
		switch {
		case info.Mode().IsRegular():
			r.Kind = "file"
			if info.Size() < 0 || info.Size() > maxArchiveBytes-total {
				return fmt.Errorf("归档数据量超过安全上限: %s", path)
			}
			total += info.Size()
			if hash {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				h := sha256.New()
				n, copyErr := io.CopyN(h, f, info.Size())
				var extra [1]byte
				more, tailErr := f.Read(extra[:])
				after, statErr := f.Stat()
				closeErr := f.Close()
				if err := errors.Join(copyErr, statErr, closeErr); err != nil {
					return err
				}
				if n != info.Size() || more != 0 || tailErr != io.EOF || !os.SameFile(info, after) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
					return fmt.Errorf("归档期间文件变化: %s", path)
				}
				r.SHA256 = hex.EncodeToString(h.Sum(nil))
			}
		case info.IsDir():
			r.Kind = "directory"
			r.Size = 0
		case info.Mode()&os.ModeSymlink != 0:
			r.Kind = "symlink"
			r.Size = 0
			r.Link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("无法完整归档特殊文件 %s（%s），原数据保留", path, info.Mode())
		}
		records = append(records, r)
		return nil
	})
	return records, err
}

func sameArchiveInventory(before, after []archiveFileRecord, content bool) error {
	if len(before) != len(after) {
		return errors.New("归档期间文件数量变化，原数据保留")
	}
	for i, b := range before {
		a := after[i]
		if b.Path != a.Path || b.Kind != a.Kind || b.Size != a.Size || b.Mode != a.Mode || b.Link != a.Link || (b.Kind == "file" && !b.Modified.Equal(a.Modified)) || (content && b.SHA256 != a.SHA256) {
			return fmt.Errorf("归档期间文件变化: %s", b.Path)
		}
	}
	return nil
}

func archiveRelativePath(name string) (string, error) {
	if strings.HasPrefix(name, "/") || name == "" {
		return "", fmt.Errorf("非法归档路径 %q", name)
	}
	if runtime.GOOS == "windows" && strings.Contains(name, "\\") {
		return "", fmt.Errorf("非法归档路径 %q", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.VolumeName(clean) != "" {
		return "", fmt.Errorf("非法归档路径 %q", name)
	}
	return clean, nil
}

// Copy into a private, new directory. No task path is renamed across mounts,
// symlinks are copied as metadata without reading their targets, and a growing
// file is an error instead of a truncated, supposedly complete snapshot.
func copyArchiveTree(ctx context.Context, source, destination string, expected []archiveFileRecord) error {
	if err := os.MkdirAll(destination, archiveDirMode); err != nil {
		return err
	}
	src, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer dst.Close()
	for _, record := range expected {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := archiveRelativePath(record.Path)
		if err != nil {
			return err
		}
		switch record.Kind {
		case "directory":
			if err := dst.MkdirAll(rel, archiveDirMode); err != nil {
				return err
			}
		case "symlink":
			if err := dst.MkdirAll(filepath.Dir(rel), archiveDirMode); err != nil {
				return err
			}
			if err := dst.Symlink(record.Link, rel); err != nil {
				return fmt.Errorf("保存符号链接 %s: %w", record.Path, err)
			}
		case "file":
			if err := dst.MkdirAll(filepath.Dir(rel), archiveDirMode); err != nil {
				return err
			}
			input, err := src.Open(rel)
			if err != nil {
				return err
			}
			before, err := input.Stat()
			if err != nil {
				input.Close()
				return err
			}
			if !before.Mode().IsRegular() || before.Size() != record.Size || !before.ModTime().Equal(record.Modified) {
				input.Close()
				return fmt.Errorf("归档前文件变化: %s", record.Path)
			}
			output, err := dst.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, archiveFileMode)
			if err != nil {
				input.Close()
				return err
			}
			h := sha256.New()
			n, copyErr := io.CopyN(io.MultiWriter(output, h), input, record.Size)
			var extra [1]byte
			more, readErr := input.Read(extra[:])
			after, statErr := input.Stat()
			closeIn := input.Close()
			syncErr := output.Sync()
			closeOut := output.Close()
			if err := errors.Join(copyErr, statErr, closeIn, syncErr, closeOut); err != nil {
				return fmt.Errorf("复制归档文件 %s: %w", record.Path, err)
			}
			if n != record.Size || more != 0 || readErr != io.EOF || after.Size() != before.Size() || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
				return fmt.Errorf("归档期间文件追加或覆盖: %s", record.Path)
			}
			if record.SHA256 != "" && record.SHA256 != hex.EncodeToString(h.Sum(nil)) {
				return fmt.Errorf("归档文件内容变化: %s", record.Path)
			}
			// Original execution bits are retained in the private archive. The
			// enclosing 0700 directory prevents other users reading the payload.
			if err := dst.Chmod(rel, fs.FileMode(record.Mode)); err != nil {
				return err
			}
			if err := dst.Chtimes(rel, record.Modified, record.Modified); err != nil {
				return err
			}
		default:
			return fmt.Errorf("不支持的归档条目 %s", record.Path)
		}
	}
	// Directory mtimes change as children are created. Apply them last, in reverse.
	for i := len(expected) - 1; i >= 0; i-- {
		r := expected[i]
		if r.Kind == "directory" {
			if err := dst.Chmod(filepath.FromSlash(r.Path), fs.FileMode(r.Mode)); err != nil {
				return err
			}
			if err := dst.Chtimes(filepath.FromSlash(r.Path), r.Modified, r.Modified); err != nil {
				return err
			}
		}
	}
	return nil
}

func sealDirectoryArchive(path string) (checksum string, size int64, err error) {
	records, err := archiveInventory(path, true)
	if err != nil {
		return "", 0, err
	}
	inventory, err := os.OpenFile(filepath.Join(path, archiveInventoryName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, archiveFileMode)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	encoder := json.NewEncoder(io.MultiWriter(inventory, h))
	for _, r := range records {
		if r.Kind == "directory" {
			if err = syncArchiveDirectory(filepath.Join(path, filepath.FromSlash(r.Path))); err != nil {
				inventory.Close()
				return "", 0, err
			}
		}
		if r.Path == "." {
			continue
		}
		if err = encoder.Encode(r); err != nil {
			inventory.Close()
			return "", 0, err
		}
		size += r.Size
	}
	err = errors.Join(inventory.Sync(), inventory.Close())
	if err != nil {
		return "", 0, err
	}
	checksum = hex.EncodeToString(h.Sum(nil))
	if err = os.WriteFile(filepath.Join(path, archiveCompleteName), []byte(checksum+"\n"), archiveFileMode); err != nil {
		return "", 0, err
	}
	marker, err := os.OpenFile(filepath.Join(path, archiveCompleteName), os.O_WRONLY, 0)
	if err != nil {
		return "", 0, err
	}
	err = errors.Join(marker.Sync(), marker.Close(), syncArchiveDirectory(path))
	return checksum, size, err
}

func verifyDirectoryArchive(path, expected string) error {
	marker, err := os.ReadFile(filepath.Join(path, archiveCompleteName))
	if err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(path, archiveInventoryName))
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	decoder := json.NewDecoder(bufio.NewReader(io.TeeReader(f, h)))
	var recorded []archiveFileRecord
	seen := map[string]bool{}
	for {
		var r archiveFileRecord
		err := decoder.Decode(&r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if _, err := archiveRelativePath(r.Path); err != nil {
			return err
		}
		if r.Path == "." || seen[r.Path] || r.Path == archiveInventoryName || r.Path == archiveCompleteName {
			return fmt.Errorf("重复或非法归档条目 %s", r.Path)
		}
		seen[r.Path] = true
		recorded = append(recorded, r)
	}
	actualHash := hex.EncodeToString(h.Sum(nil))
	if actualHash != strings.TrimSpace(string(marker)) || (expected != "" && actualHash != expected) {
		return errors.New("归档清单校验失败")
	}
	actual, err := archiveInventory(path, true)
	if err != nil {
		return err
	}
	filtered := actual[:0]
	for _, r := range actual {
		if r.Path != "." && r.Path != archiveInventoryName && r.Path != archiveCompleteName {
			filtered = append(filtered, r)
		}
	}
	// Inventory traversal order is stable, independently of the JSON record order.
	sort.Slice(recorded, func(i, j int) bool { return recorded[i].Path < recorded[j].Path })
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Path < filtered[j].Path })
	return sameArchiveInventory(recorded, filtered, true)
}

func archiveRequiredSpace(estimated uint64, entries int64, block uint64) (uint64, error) {
	if estimated > uint64(maxArchiveBytes) || entries < 0 || entries > maxArchiveFiles {
		return 0, errors.New("归档数据量超过安全上限")
	}
	if block == 0 {
		block = 4096
	}
	if block > 1<<30 {
		return 0, errors.New("文件系统分配块大小无效")
	}
	// Per-entry allocation + directory/inventory overhead and a 10% reserve;
	// account for minimum disk blocks, rather than treating empty files as free.
	overhead := uint64(entries+16) * block * 2
	if estimated > ^uint64(0)-overhead {
		return 0, errors.New("归档大小超出可计算范围")
	}
	total := estimated + overhead
	reserve := total / 10
	if reserve < 64<<20 {
		reserve = 64 << 20
	}
	if total > ^uint64(0)-reserve {
		return 0, errors.New("归档空间估计溢出")
	}
	return total + reserve, nil
}
