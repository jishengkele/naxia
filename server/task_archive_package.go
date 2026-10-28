package server

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/config"
	pgdb "github.com/Autumn-27/artex/db"
	"github.com/klauspost/compress/zstd"
)

const (
	archiveDirMode  = 0o700
	archiveFileMode = 0o600
	maxArchiveFiles = 1_000_000
	maxArchiveBytes = int64(1 << 47) // 128 TiB safety ceiling for corrupt headers.
)

type archiveFileMove struct {
	Source   string `json:"source"`
	Relative string `json:"relative"`
}

type archiveStageJournal struct {
	ArchiveID int64             `json:"archive_id"`
	TaskID    string            `json:"task_id"`
	Moves     []archiveFileMove `json:"moves"`
}

type archiveRestoreJournal struct {
	ArchiveID int64                 `json:"archive_id"`
	TaskID    int64                 `json:"task_id"`
	Moves     []restoredArchivePath `json:"moves"`
}

type taskArchiveFileStage struct {
	root    string
	payload string
	journal archiveStageJournal
	done    bool
}

type restoredArchivePath struct {
	Source      string
	Destination string
}

type taskArchiveRestoreFiles struct {
	extracted string
	moves     []restoredArchivePath
	done      bool
}

func archiveJournalDataPath(dataDir, relative, taskID string) bool {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) != 2 || parts[1] == "" || parts[1] == "." || parts[1] == ".." {
		return false
	}
	if parts[0] == "tasks" {
		return parts[1] == taskID
	}
	return parts[0] == "transcripts" || parts[0] == "sessions" || parts[0] == "noa"
}

func validateArchiveStageJournal(dataDir string, journal archiveStageJournal) error {
	id, err := strconv.ParseInt(journal.TaskID, 10, 64)
	if err != nil || id <= 0 || journal.ArchiveID <= 0 {
		return errors.New("归档暂存日志身份无效")
	}
	base, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	for _, move := range journal.Moves {
		source, err := filepath.Abs(move.Source)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, source)
		if err != nil || !archiveJournalDataPath(dataDir, rel, journal.TaskID) || filepath.Clean(move.Relative) != filepath.Join("files", rel) {
			return errors.New("归档暂存日志路径越界，原数据保留")
		}
	}
	return nil
}

func validateArchiveRestoreJournal(dataDir, work string, journal archiveRestoreJournal) error {
	if journal.TaskID <= 0 || journal.ArchiveID <= 0 {
		return errors.New("恢复暂存日志身份无效")
	}
	base, err := filepath.Abs(filepath.Join(work, "files"))
	if err != nil {
		return err
	}
	data, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	for _, move := range journal.Moves {
		source, err := filepath.Abs(move.Source)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, source)
		destination, e := filepath.Abs(move.Destination)
		if err != nil || e != nil || !archiveJournalDataPath(dataDir, rel, strconv.FormatInt(journal.TaskID, 10)) || destination != filepath.Join(data, rel) {
			return errors.New("恢复暂存日志路径越界，文件保留")
		}
	}
	return nil
}

func writeArchiveJournal(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".journal-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncArchiveDirectory(filepath.Dir(path))
}

func taskArchiveRoot(dataDir string) string {
	if base := config.ArchiveDir(); base != "" {
		return filepath.Join(base, "tasks")
	}
	return filepath.Join(dataDir, "archives", "tasks")
}

func taskArchivePath(dataDir string, archiveID int64, taskID string) string {
	return filepath.Join(taskArchiveRoot(dataDir), fmt.Sprintf("task-%s-%d.tar.zst", taskID, archiveID))
}

func stageTaskArchiveFiles(dataDir string, archiveID int64, taskID string, explorationID int64) (*taskArchiveFileStage, error) {
	root := filepath.Join(dataDir, "archives", "tasks", ".staging", strconv.FormatInt(archiveID, 10))
	stage := &taskArchiveFileStage{root: root, payload: filepath.Join(root, "payload")}
	journalPath := filepath.Join(root, "journal.json")
	if raw, err := os.ReadFile(journalPath); err == nil {
		if err := json.Unmarshal(raw, &stage.journal); err != nil {
			return nil, fmt.Errorf("read task archive staging journal: %w", err)
		}
		// The journal is written before the first rename, so its presence does not mean
		// the payload is complete: a previous round may have failed mid-loop with a
		// rollback that itself errored, which leaves the root in place. Replay the moves
		// rather than packaging a payload that is missing transcripts or workspace files.
		if err := os.MkdirAll(filepath.Join(stage.payload, "files"), archiveDirMode); err != nil {
			return nil, err
		}
		if err := stage.applyMoves(); err != nil {
			_ = stage.rollback()
			return nil, err
		}
		return stage, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(stage.payload, "files"), archiveDirMode); err != nil {
		return nil, err
	}
	stage.journal = archiveStageJournal{ArchiveID: archiveID, TaskID: taskID}
	targets := []archiveFileMove{}
	taskDir := filepath.Join(dataDir, "tasks", taskID)
	if _, err := os.Lstat(taskDir); err == nil {
		targets = append(targets, archiveFileMove{Source: taskDir, Relative: filepath.Join("files", "tasks", taskID)})
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	transcriptDir := filepath.Join(dataDir, "transcripts")
	entries, err := os.ReadDir(transcriptDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	prefix := fmt.Sprintf("exp%d-", explorationID)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || (!entry.IsDir() && !strings.HasSuffix(name, ".jsonl")) {
			continue
		}
		targets = append(targets, archiveFileMove{
			Source: filepath.Join(transcriptDir, name), Relative: filepath.Join("files", "transcripts", name),
		})
	}
	// Persist the complete plan before the first rename. Recovery can therefore
	// roll back any prefix of the moves after an abrupt process termination.
	stage.journal.Moves = targets
	if err := writeArchiveJournal(journalPath, stage.journal); err != nil {
		_ = stage.rollback()
		return nil, err
	}
	if err := stage.applyMoves(); err != nil {
		_ = stage.rollback()
		return nil, err
	}
	return stage, nil
}

// applyMoves performs the renames recorded in the journal. Entries already sitting
// in the payload are skipped, so an interrupted staging round can be resumed in
// place without moving anything twice.
func (s *taskArchiveFileStage) applyMoves() error {
	for _, move := range s.journal.Moves {
		destination := filepath.Join(s.payload, move.Relative)
		if _, err := os.Lstat(destination); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if _, err := os.Lstat(move.Source); err != nil {
			if os.IsNotExist(err) {
				// Neither side exists: the path was removed outside the archive flow
				// after the journal was written. Nothing can be staged for it.
				continue
			}
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), archiveDirMode); err != nil {
			return err
		}
		if err := os.Rename(move.Source, destination); err != nil {
			return fmt.Errorf("stage task archive path %s: %w", move.Source, err)
		}
	}
	return nil
}

func (s *taskArchiveFileStage) rollback() error {
	if s == nil || s.done {
		return nil
	}
	var errs []error
	for i := len(s.journal.Moves) - 1; i >= 0; i-- {
		move := s.journal.Moves[i]
		staged := filepath.Join(s.payload, move.Relative)
		if _, err := os.Lstat(staged); os.IsNotExist(err) {
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := os.Lstat(move.Source); err == nil {
			errs = append(errs, fmt.Errorf("archive rollback destination exists: %s", move.Source))
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(move.Source), 0o755); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(staged, move.Source); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		if err := os.RemoveAll(s.root); err != nil {
			errs = append(errs, err)
		}
	}
	s.done = len(errs) == 0
	return errors.Join(errs...)
}

func (s *taskArchiveFileStage) commit() error {
	if s == nil || s.done {
		return nil
	}
	s.done = true
	return os.RemoveAll(s.root)
}

func installTaskArchiveFiles(dataDir, extractedDir, taskID string, archiveID int64, reuseExisting ...bool) (*taskArchiveRestoreFiles, error) {
	stage := &taskArchiveRestoreFiles{extracted: extractedDir}
	numericTaskID, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil || numericTaskID <= 0 {
		return nil, fmt.Errorf("invalid restore task id %q", taskID)
	}
	sources := []restoredArchivePath{}
	workspace := filepath.Join(extractedDir, "files", "tasks", taskID)
	if _, err := os.Lstat(workspace); err == nil {
		sources = append(sources, restoredArchivePath{Source: workspace, Destination: filepath.Join(dataDir, "tasks", taskID)})
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	transcripts := filepath.Join(extractedDir, "files", "transcripts")
	entries, err := os.ReadDir(transcripts)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		sources = append(sources, restoredArchivePath{
			Source: filepath.Join(transcripts, entry.Name()), Destination: filepath.Join(dataDir, "transcripts", entry.Name()),
		})
	}
	for _, base := range []string{"sessions", "noa"} {
		entries, err := os.ReadDir(filepath.Join(extractedDir, "files", base))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			sources = append(sources, restoredArchivePath{Source: filepath.Join(extractedDir, "files", base, entry.Name()), Destination: filepath.Join(dataDir, base, entry.Name())})
		}
	}
	filtered := sources[:0]
	for _, move := range sources {
		if _, err := os.Lstat(move.Destination); err == nil {
			if len(reuseExisting) > 0 && reuseExisting[0] {
				before, e := archiveInventory(move.Source, true)
				if e != nil {
					return nil, e
				}
				after, e := archiveInventory(move.Destination, true)
				if e != nil {
					return nil, e
				}
				if e = sameArchiveInventory(before, after, true); e == nil {
					continue
				}
			}
			return nil, fmt.Errorf("restore destination already exists: %s", move.Destination)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		filtered = append(filtered, move)
	}
	sources = filtered
	stage.moves = sources
	journal := archiveRestoreJournal{ArchiveID: archiveID, TaskID: numericTaskID, Moves: sources}
	if err := writeArchiveJournal(filepath.Join(extractedDir, "restore-journal.json"), journal); err != nil {
		return nil, err
	}
	for _, move := range sources {
		if err := os.MkdirAll(filepath.Dir(move.Destination), 0o755); err != nil {
			return nil, errors.Join(err, stage.rollback())
		}
		if err := os.Rename(move.Source, move.Destination); err != nil {
			return nil, errors.Join(err, stage.rollback())
		}
	}
	return stage, nil
}

func (s *taskArchiveRestoreFiles) rollback() error {
	if s == nil || s.done {
		return nil
	}
	var errs []error
	for i := len(s.moves) - 1; i >= 0; i-- {
		move := s.moves[i]
		if _, err := os.Lstat(move.Destination); os.IsNotExist(err) {
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := os.Lstat(move.Source); err == nil {
			errs = append(errs, fmt.Errorf("restore rollback source exists: %s", move.Source))
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(move.Source), archiveDirMode); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(move.Destination, move.Source); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("恢复文件回滚未完成，暂存目录保留 %s: %w", s.extracted, errors.Join(errs...))
	}
	if err := os.Remove(filepath.Join(s.extracted, "restore-journal.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	s.done = true
	return nil
}

// A failed rollback still needs both its payload and journal for recovery.
func cleanupTaskArchiveRestoreWork(work string) error {
	if _, err := os.Lstat(filepath.Join(work, "restore-journal.json")); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(work)
}

func (s *taskArchiveRestoreFiles) commit() error {
	if s == nil || s.done {
		return nil
	}
	s.done = true
	return os.RemoveAll(s.extracted)
}

// recoverTaskArchiveRestoreStages resolves file installs left by an interrupted
// restore. If PostgreSQL committed, installed files are authoritative and only
// the extraction directory is stale. Otherwise all completed renames are moved
// back so the persistent restore job can retry from a clean destination.
func recoverTaskArchiveRestoreStages(dataDir string, pg *pgdb.DB, onlyIDs ...int64) error {
	parent := filepath.Join(dataDir, "archives", "tasks", ".restore")
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if len(onlyIDs) > 0 && !strings.HasPrefix(entry.Name(), fmt.Sprintf("%d-", onlyIDs[0])) {
			continue
		}
		root := filepath.Join(parent, entry.Name())
		raw, err := os.ReadFile(filepath.Join(root, "restore-journal.json"))
		if os.IsNotExist(err) {
			// Extraction was interrupted before any destination rename.
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var journal archiveRestoreJournal
		if err := json.Unmarshal(raw, &journal); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := validateArchiveRestoreJournal(dataDir, root, journal); err != nil {
			errs = append(errs, err)
			continue
		}
		if !strings.HasPrefix(entry.Name(), fmt.Sprintf("%d-", journal.ArchiveID)) {
			errs = append(errs, errors.New("恢复日志与暂存目录身份不一致"))
			continue
		}
		archive, err := pg.GetTaskArchive(journal.ArchiveID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if archive != nil && archive.TaskID != journal.TaskID {
			errs = append(errs, errors.New("恢复日志任务身份不一致"))
			continue
		}
		restored, err := pg.IsTaskArchiveRestored(journal.ArchiveID)
		if errors.Is(err, pgdb.ErrTaskArchiveNotFound) && journal.TaskID > 0 {
			task, taskErr := pg.GetTask(journal.TaskID)
			if taskErr != nil {
				errs = append(errs, taskErr)
				continue
			}
			restored = task != nil
			err = nil
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if restored {
			for _, move := range journal.Moves {
				_, sourceErr := os.Lstat(move.Source)
				_, destErr := os.Lstat(move.Destination)
				if sourceErr != nil && !os.IsNotExist(sourceErr) {
					err = sourceErr
					break
				}
				if destErr != nil && !os.IsNotExist(destErr) {
					err = destErr
					break
				}
				if os.IsNotExist(sourceErr) && os.IsNotExist(destErr) {
					err = fmt.Errorf("数据库已恢复，但恢复文件两端均缺失: %s", move.Destination)
					break
				}
				if sourceErr == nil && os.IsNotExist(destErr) {
					if err = os.MkdirAll(filepath.Dir(move.Destination), 0700); err == nil {
						err = os.Rename(move.Source, move.Destination)
					}
					if err != nil {
						break
					}
				}
				if sourceErr == nil && destErr == nil {
					before, e := archiveInventory(move.Source, true)
					if e != nil {
						err = e
						break
					}
					after, e := archiveInventory(move.Destination, true)
					if e != nil {
						err = e
						break
					}
					if e = sameArchiveInventory(before, after, true); e != nil {
						err = fmt.Errorf("数据库已恢复，但文件副本冲突，暂存保留: %w", e)
						break
					}
				}
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		stage := &taskArchiveRestoreFiles{extracted: root, moves: journal.Moves}
		if err := stage.rollback(); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.RemoveAll(root); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recoverTaskArchiveStages resolves file renames left by an interrupted archive.
// A committed cold task already has a verified package, so stale staging can be
// discarded; otherwise files are moved back before the persistent job retries.
func recoverTaskArchiveStages(dataDir string, pg *pgdb.DB, onlyIDs ...int64) error {
	parent := filepath.Join(dataDir, "archives", "tasks", ".staging")
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if len(onlyIDs) > 0 && entry.Name() != strconv.FormatInt(onlyIDs[0], 10) {
			continue
		}
		root := filepath.Join(parent, entry.Name())
		raw, err := os.ReadFile(filepath.Join(root, "journal.json"))
		if os.IsNotExist(err) {
			// A missing journal is not proof that a nonempty payload is disposable.
			payload, scanErr := archiveInventory(filepath.Join(root, "payload"), false)
			if scanErr != nil && !os.IsNotExist(scanErr) {
				errs = append(errs, scanErr)
				continue
			}
			hasData := false
			for _, record := range payload {
				if record.Kind != "directory" || (record.Path != "." && record.Path != "files") {
					hasData = true
					break
				}
			}
			if hasData {
				id, parseErr := strconv.ParseInt(entry.Name(), 10, 64)
				committed := false
				if parseErr == nil {
					archive, getErr := pg.GetTaskArchive(id)
					if getErr != nil {
						errs = append(errs, getErr)
						continue
					}
					if archive != nil {
						if getErr = pg.QueryRow(`SELECT archived_at IS NOT NULL FROM tasks WHERE id=$1`, archive.TaskID).Scan(&committed); getErr != nil {
							errs = append(errs, getErr)
							continue
						}
						if committed {
							if getErr = verifyCommittedArchiveCopy(dataDir, archive, filepath.Join(root, "payload")); getErr != nil {
								errs = append(errs, getErr)
								continue
							}
						}
					}
				}
				if !committed {
					errs = append(errs, fmt.Errorf("归档暂存日志缺失，未知原始数据已保留: %s", root))
					continue
				}
			}
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var journal archiveStageJournal
		if err := json.Unmarshal(raw, &journal); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := validateArchiveStageJournal(dataDir, journal); err != nil {
			errs = append(errs, err)
			continue
		}
		if entry.Name() != strconv.FormatInt(journal.ArchiveID, 10) {
			errs = append(errs, errors.New("归档日志与暂存目录身份不一致"))
			continue
		}
		archive, getErr := pg.GetTaskArchive(journal.ArchiveID)
		if getErr != nil {
			errs = append(errs, getErr)
			continue
		}
		compacted := false
		if archive != nil {
			if strconv.FormatInt(archive.TaskID, 10) != journal.TaskID {
				errs = append(errs, errors.New("归档暂存任务身份不一致"))
				continue
			}
			if err := pg.QueryRow(`SELECT archived_at IS NOT NULL FROM tasks WHERE id=$1`, archive.TaskID).Scan(&compacted); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if compacted {
			if err := verifyCommittedArchiveCopy(dataDir, archive, filepath.Join(root, "payload")); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		stage := &taskArchiveFileStage{root: root, payload: filepath.Join(root, "payload"), journal: journal}
		if err := stage.rollback(); err != nil {
			errs = append(errs, err)
		}
		if archive != nil && archive.ArchivePath != "" && archive.StorageKind != directoryArchiveKind {
			_ = os.Remove(archive.ArchivePath)
		}
	}
	return errors.Join(errs...)
}

// Never discard the last original merely because SQL says "committed". A lost
// or damaged cold copy must leave its original staging available for recovery.
func verifyCommittedArchiveCopy(dataDir string, archive *pgdb.TaskArchive, payload string) error {
	if err := validateArchivePath(dataDir, archive.ArchivePath); err != nil {
		return err
	}
	if archive.StorageKind == directoryArchiveKind {
		if err := verifyDirectoryArchive(archive.ArchivePath, archive.SHA256); err != nil {
			return fmt.Errorf("完整归档副本不可用，原始暂存保留: %w", err)
		}
		remaining, err := archiveInventory(payload, true)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		copied, err := archiveInventory(archive.ArchivePath, true)
		if err != nil {
			return err
		}
		byPath := map[string]archiveFileRecord{}
		for _, record := range copied {
			byPath[record.Path] = record
		}
		for _, record := range remaining {
			if record.Path == "." {
				continue
			}
			saved, ok := byPath[record.Path]
			if !ok {
				return fmt.Errorf("原始暂存含未归档数据，已保留: %s", record.Path)
			}
			if err = sameArchiveInventory([]archiveFileRecord{record}, []archiveFileRecord{saved}, true); err != nil {
				return err
			}
		}
		return nil
	}
	f, err := os.Open(archive.ArchivePath)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, readErr := io.Copy(h, f)
	if err = errors.Join(readErr, f.Close()); err != nil {
		return err
	}
	if archive.SHA256 == "" || hex.EncodeToString(h.Sum(nil)) != archive.SHA256 {
		return errors.New("归档包校验失败，原始暂存保留")
	}
	return nil
}

func recoverTaskArchiveDeletePackages(dataDir string, pg *pgdb.DB) error {
	var errs []error
	seen := map[string]bool{}
	for _, root := range []string{taskArchiveRoot(dataDir), filepath.Join(dataDir, "archives", "tasks")} {
		if !seen[root] {
			seen[root] = true
			errs = append(errs, recoverTaskArchiveDeleteRoot(dataDir, root, pg))
		}
	}
	return errors.Join(errs...)
}

func recoverTaskArchiveDeleteRoot(dataDir, root string, pg *pgdb.DB) error {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		marker := strings.LastIndex(name, ".deleting-")
		if marker < 0 {
			if entry.IsDir() && strings.HasPrefix(name, "task-") {
				if id, err := strconv.ParseInt(strings.TrimPrefix(name, "task-"), 10, 64); err == nil && id > 0 {
					errs = append(errs, recoverTaskArchiveDeleteRoot(dataDir, filepath.Join(root, name), pg))
				}
			}
			continue
		}
		archiveID, err := strconv.ParseInt(name[marker+len(".deleting-"):], 10, 64)
		if err != nil || archiveID <= 0 {
			continue
		}
		staged := filepath.Join(root, name)
		original := filepath.Join(root, name[:marker])
		archive, err := pg.GetTaskArchive(archiveID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if archive == nil {
			if err := removeManagedArchive(dataDir, staged); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			continue
		}
		if archive.State == pgdb.DeleteQueued || archive.State == pgdb.Deleting || archive.State == pgdb.DeleteFailed {
			// The idempotent delete worker consumes the staged path directly.
			continue
		}
		if _, err := os.Lstat(original); err == nil {
			errs = append(errs, fmt.Errorf("archive delete recovery destination exists: %s", original))
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(staged, original); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func stageTaskArchivePackageDelete(archivePath string, archiveID int64) (string, bool, error) {
	staged := archivePath + fmt.Sprintf(".deleting-%d", archiveID)
	if _, err := os.Lstat(staged); err == nil {
		if _, originalErr := os.Lstat(archivePath); originalErr == nil {
			return staged, false, errors.New("归档包原文件和删除暂存文件同时存在")
		} else if !os.IsNotExist(originalErr) {
			return staged, false, originalErr
		}
		return staged, true, nil
	} else if !os.IsNotExist(err) {
		return staged, false, err
	}
	if err := os.Rename(archivePath, staged); err == nil {
		return staged, true, nil
	} else if !os.IsNotExist(err) {
		return staged, false, err
	}
	return staged, false, nil
}

func writeTaskArchivePackage(path, payloadDir string, snapshot *pgdb.TaskArchiveSnapshot) (originalSize, compressedSize int64, checksum string, err error) {
	if snapshot == nil {
		return 0, 0, "", errors.New("nil task archive snapshot")
	}
	if err = os.MkdirAll(filepath.Dir(path), archiveDirMode); err != nil {
		return 0, 0, "", err
	}
	manifestFile, err := os.OpenFile(
		filepath.Join(payloadDir, "manifest.json"),
		os.O_CREATE|os.O_TRUNC|os.O_WRONLY,
		archiveFileMode,
	)
	if err != nil {
		return 0, 0, "", err
	}
	encodeErr := json.NewEncoder(manifestFile).Encode(snapshot)
	var syncErr error
	if encodeErr == nil {
		syncErr = manifestFile.Sync()
	}
	if err := errors.Join(encodeErr, syncErr, manifestFile.Close()); err != nil {
		return 0, 0, "", err
	}
	temporary := path + ".partial"
	_ = os.Remove(temporary)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, archiveFileMode)
	if err != nil {
		return 0, 0, "", err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(temporary)
		}
	}()
	hasher := sha256.New()
	multi := io.MultiWriter(file, hasher)
	encoder, err := zstd.NewWriter(multi,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(10)),
		zstd.WithEncoderCRC(true),
	)
	if err != nil {
		return 0, 0, "", err
	}
	tarWriter := tar.NewWriter(encoder)
	walkErr := filepath.WalkDir(payloadDir, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(payloadDir, current)
		if err != nil || relative == "." {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// 归档格式端到端只支持普通文件与目录（解包端对其它类型直接报错），
			// 无法还原符号链接。跳过而非整包失败：不读取链接目标(lstat，不越出目录树)，
			// 也不写入 symlink 条目；链接指向树内时目标文件本身仍会被单独遍历归档。
			log.Printf("[task-archive] 跳过符号链接（归档不支持，不影响其它文件）：%s", current)
			return nil
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relative)
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		originalSize += info.Size()
		input, err := os.Open(current)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(tarWriter, input, info.Size())
		var extra [1]byte
		n, tailErr := input.Read(extra[:])
		after, statErr := input.Stat()
		closeErr := input.Close()
		if err := errors.Join(copyErr, statErr, closeErr); err != nil {
			return fmt.Errorf("archive file %s: %w", current, err)
		}
		if n != 0 || tailErr != io.EOF || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
			return fmt.Errorf("archive file changed while reading: %s", current)
		}
		return nil
	})
	if walkErr != nil {
		_ = tarWriter.Close()
		_ = encoder.Close()
		return 0, 0, "", walkErr
	}
	if err := tarWriter.Close(); err != nil {
		_ = encoder.Close()
		return 0, 0, "", err
	}
	if err := encoder.Close(); err != nil {
		return 0, 0, "", err
	}
	if err := file.Sync(); err != nil {
		return 0, 0, "", err
	}
	if err := file.Close(); err != nil {
		return 0, 0, "", err
	}
	stat, err := os.Stat(temporary)
	if err != nil {
		return 0, 0, "", err
	}
	compressedSize = stat.Size()
	checksum = hex.EncodeToString(hasher.Sum(nil))
	if err := os.Rename(temporary, path); err != nil {
		return 0, 0, "", err
	}
	if err := os.Chmod(path, archiveFileMode); err != nil {
		return 0, 0, "", err
	}
	committed = true
	return originalSize, compressedSize, checksum, nil
}

func extractTaskArchivePackage(path, expectedSHA, destination string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if expectedSHA != "" {
		hasher := sha256.New()
		if _, err := io.Copy(hasher, file); err != nil {
			return err
		}
		if hex.EncodeToString(hasher.Sum(nil)) != strings.ToLower(expectedSHA) {
			return errors.New("task archive checksum mismatch")
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	decoder, err := zstd.NewReader(file)
	if err != nil {
		return err
	}
	defer decoder.Close()
	tarReader := tar.NewReader(decoder)
	if err := os.MkdirAll(destination, archiveDirMode); err != nil {
		return err
	}
	var entries int
	var total int64
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxArchiveFiles || header.Size < 0 || total+header.Size > maxArchiveBytes {
			return errors.New("task archive exceeds extraction safety limits")
		}
		total += header.Size
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe task archive path %q", header.Name)
		}
		target := filepath.Join(destination, clean)
		if relative, err := filepath.Rel(destination, target); err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe task archive target %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, archiveDirMode); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), archiveDirMode); err != nil {
				return err
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(output, tarReader, header.Size)
			closeErr := output.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported task archive entry type %d", header.Typeflag)
		}
	}
	return nil
}
