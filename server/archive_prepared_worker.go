package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

func managedArchivePath(dataDir, candidate string) (string, error) {
	if strings.TrimSpace(candidate) == "" {
		return "", errors.New("归档路径为空")
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	for _, base := range []string{taskArchiveRoot(dataDir), filepath.Join(dataDir, "archives", "tasks")} {
		root, err := filepath.Abs(base)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		probe := abs
		for {
			if _, err = os.Lstat(probe); err == nil {
				break
			} else if !os.IsNotExist(err) {
				return "", err
			}
			parent := filepath.Dir(probe)
			if parent == probe {
				return "", errors.New("归档路径无法解析")
			}
			probe = parent
		}
		resolved, err := filepath.EvalSymlinks(probe)
		if err != nil {
			return "", err
		}
		rel, err = filepath.Rel(resolvedRoot, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return abs, nil
		}
	}
	return "", errors.New("归档路径不在受管目录内，或通过符号链接越界")
}

func removeManagedArchive(dataDir, path string) error {
	checked, err := managedArchivePath(dataDir, path)
	if err != nil {
		return err
	}
	info, err := os.Lstat(checked)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return os.RemoveAll(checked)
	}
	return os.Remove(checked)
}

func (s *Server) archivePreparedTask(job *db.TaskArchive) (runErr error) {
	s.archivePrepareMu.Lock()
	defer s.archivePrepareMu.Unlock()
	taskID := strconv.FormatInt(job.TaskID, 10)
	if !s.beginTaskDelete(taskID) {
		return errors.New("任务正在执行其他归档或删除操作")
	}
	committed := false
	defer func() {
		if !committed {
			s.abortTaskDelete(taskID)
		}
	}()
	if err := validateArchivePath(s.m.dir, job.PreparedPath); err != nil {
		return err
	}
	if err := verifyDirectoryArchive(job.PreparedPath, job.SHA256); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(job.PreparedPath, "source-plan.json"))
	if err != nil {
		return err
	}
	var plan archiveAdmissionPlan
	if err = json.Unmarshal(raw, &plan); err != nil {
		return err
	}
	manifest, err := os.ReadFile(filepath.Join(job.PreparedPath, "manifest.json"))
	if err != nil {
		return err
	}
	var snapshot db.TaskArchiveSnapshot
	if err = json.Unmarshal(manifest, &snapshot); err != nil {
		return err
	}
	if plan.TaskID != job.TaskID || snapshot.TaskID != job.TaskID || plan.ExplorationID != snapshot.ExplorationID || snapshot.SnapshotAt.IsZero() || snapshot.FormatVersion != db.TaskArchiveFormatVersion {
		return db.ErrTaskArchiveFormatMismatch
	}
	base, err := filepath.Abs(s.m.dir)
	if err != nil {
		return err
	}
	for _, source := range plan.Sources {
		rel, err := filepath.Rel(base, source.Source)
		valid := err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && source.Relative == filepath.ToSlash(filepath.Join("files", rel))
		if !valid && s.skillDir != "" && !source.Owned {
			skillRoot, e := filepath.Abs(s.skillDir)
			if e == nil {
				skillRel, e := filepath.Rel(skillRoot, source.Source)
				valid = e == nil && validSkillName(skillRel) && source.Relative == "dependencies/skills/"+skillRel
			}
		}
		if !valid {
			return errors.New("归档原始数据路径校验失败")
		}
		actual, err := archiveInventory(source.Source, true)
		if err != nil {
			return err
		}
		if err = sameArchiveInventory(source.Entries, actual, true); err != nil {
			return err
		}
	}
	if err = archiveCheckWriters(archiveSourceRoots(plan.Sources)); err != nil {
		return err
	}
	_ = s.m.pg.UpdateTaskArchiveProgress(job.ID, "verify_snapshot", 60)
	// Only task-owned trees move, and only within the original data mount. The
	// complete immutable copy is already on the destination filesystem.
	stageRoot := filepath.Join(s.m.dir, "archives", "tasks", ".staging", strconv.FormatInt(job.ID, 10))
	if _, err := os.Lstat(stageRoot); err == nil {
		return errors.New("旧归档暂存尚未恢复，已保留原数据")
	} else if !os.IsNotExist(err) {
		return err
	}
	stage := &taskArchiveFileStage{root: stageRoot, payload: filepath.Join(stageRoot, "payload"), journal: archiveStageJournal{ArchiveID: job.ID, TaskID: taskID}}
	for _, source := range plan.Sources {
		if source.Owned {
			stage.journal.Moves = append(stage.journal.Moves, archiveFileMove{Source: source.Source, Relative: filepath.FromSlash(source.Relative)})
		}
	}
	if err = os.MkdirAll(stage.payload, archiveDirMode); err != nil {
		return err
	}
	if err = writeArchiveJournal(filepath.Join(stageRoot, "journal.json"), stage.journal); err != nil {
		return err
	}
	defer func() {
		if !committed {
			runErr = errors.Join(runErr, stage.rollback())
		}
	}()
	if err = stage.applyMoves(); err != nil {
		return err
	}
	var stagedRoots []string
	for _, source := range plan.Sources {
		path := source.Source
		if source.Owned {
			path = filepath.Join(stage.payload, filepath.FromSlash(source.Relative))
		}
		stagedRoots = append(stagedRoots, path)
		actual, err := archiveInventory(path, true)
		if err != nil {
			return err
		}
		if err = sameArchiveInventory(source.Entries, actual, true); err != nil {
			return err
		}
	}
	if err = archiveCheckWriters(stagedRoots); err != nil {
		return err
	}
	var size int64
	entries, err := archiveInventory(job.PreparedPath, false)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		size += entry.Size
	}
	_ = s.m.pg.UpdateTaskArchiveProgress(job.ID, "commit_archive", 85)
	// The database implementation locks and revalidates every exported table in
	// this same transaction before removing any task-owned hot rows.
	if err = s.m.pg.CompleteTaskArchive(job.ID, &snapshot, job.PreparedPath, job.SHA256, size, size); err != nil {
		return err
	}
	committed = true
	if err = stage.commit(); err != nil {
		_ = s.m.pg.AppendTaskArchiveWarning(job.ID, "归档已保存；原数据暂存清理待恢复："+err.Error())
	}
	// Global traffic is retained: deleting by host could remove captures made
	// after the cutoff or used by other tasks. The archive contains its own copy.
	s.engine.StopTask(taskID)
	s.taskAgentMu.Lock()
	delete(s.taskAgents, taskID)
	s.taskAgentMu.Unlock()
	s.m.forgetTask(taskID, job.TaskID)
	return nil
}

func (s *Server) restoreDirectoryArchive(job *db.TaskArchive) (runErr error) {
	if err := validateArchivePath(s.m.dir, job.ArchivePath); err != nil {
		return err
	}
	if err := recoverTaskArchiveStages(s.m.dir, s.m.pg, job.ID); err != nil {
		return err
	}
	if err := recoverTaskArchiveRestoreStages(s.m.dir, s.m.pg, job.ID); err != nil {
		return err
	}
	if restored, err := s.m.pg.IsTaskArchiveRestored(job.ID); err != nil {
		return err
	} else if restored {
		if err := s.m.pg.CompleteTaskArchiveRestore(job.ID); err != nil {
			return err
		}
		s.loadRestoredTask(job.TaskID)
		return nil
	}
	if err := verifyDirectoryArchive(job.ArchivePath, job.SHA256); err != nil {
		return err
	}
	entries, err := archiveInventory(job.ArchivePath, true)
	if err != nil {
		return err
	}
	if err = s.checkDirectoryRestoreCapacity(entries); err != nil {
		return err
	}
	// Extraction stays on the data mount, so installation never attempts an
	// invalid rename across the archive/data bind mounts.
	parent := filepath.Join(s.m.dir, "archives", "tasks", ".restore")
	if err := os.MkdirAll(parent, archiveDirMode); err != nil {
		return err
	}
	work, err := os.MkdirTemp(parent, fmt.Sprintf("%d-", job.ID))
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, cleanupTaskArchiveRestoreWork(work)) }()
	if err := copyArchiveTree(s.ctx, job.ArchivePath, work, entries); err != nil {
		return err
	}
	manifest, err := os.ReadFile(filepath.Join(work, "manifest.json"))
	if err != nil {
		return err
	}
	var snapshot db.TaskArchiveSnapshot
	if err = json.Unmarshal(manifest, &snapshot); err != nil {
		return err
	}
	if snapshot.TaskID != job.TaskID || !db.IsTaskArchiveFormatSupported(snapshot.FormatVersion) {
		return db.ErrTaskArchiveFormatMismatch
	}
	proof, err := db.ArchiveEvidenceSnapshots(&snapshot)
	if err != nil {
		return err
	}
	return s.evidenceStore().WithInstalledSnapshots(s.ctx, proof, filepath.Join(work, "evidence"), func() error { return s.restoreTaskArchivePayload(job, &snapshot, work) })
}

// The archive filesystem may have plenty of free space while the data mount is
// full. Budget the extraction plus a second copy for imported evidence/traffic;
// installation itself only renames files within the data mount.
func (s *Server) checkDirectoryRestoreCapacity(entries []archiveFileRecord) error {
	var bytes uint64
	var trafficIndex uint64
	for _, entry := range entries {
		if entry.Size < 0 || uint64(entry.Size) > uint64(maxArchiveBytes)-bytes {
			return errors.New("恢复数据量超过安全上限")
		}
		bytes += uint64(entry.Size)
		if entry.Path == "traffic/exchanges.ndjson" || entry.Path == "traffic/traffic.json" {
			trafficIndex += uint64(entry.Size)
		}
	}
	query := s.archiveSpaceQuery
	if query == nil {
		query = archiveDiskSpace
	}
	available, block, err := query(s.m.dir)
	if err != nil {
		return fmt.Errorf("读取恢复目标盘空间: %w", err)
	}
	// SQLite rows, FTS indexes and WAL can coexist during import. Raw JSON
	// bytes alone are not the import's peak allocation.
	if trafficIndex > uint64(maxArchiveBytes)/8 || bytes > uint64(maxArchiveBytes)-trafficIndex*8 {
		return errors.New("恢复流量临时空间超过安全上限")
	}
	required, err := archiveRequiredSpace(bytes+trafficIndex*8, int64(len(entries)), block)
	if err != nil {
		return err
	}
	required *= 2 // Limits apply to each copy, not to the combined temporary peak.
	estimate := archiveSpaceEstimate{EstimatedBytes: (bytes + trafficIndex*8) * 2, RequiredBytes: required, AvailableBytes: available, FileCount: int64(len(entries)) * 2, Sufficient: available >= required}
	if !estimate.Sufficient {
		return &archiveCapacityError{Estimate: estimate, Operation: "恢复"}
	}
	return nil
}
