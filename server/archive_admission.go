package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
)

type archiveAdmissionPlan struct {
	TaskID         int64                   `json:"task_id"`
	ExplorationID  int64                   `json:"exploration_id"`
	SnapshotAt     time.Time               `json:"snapshot_at"`
	RequestedAt    time.Time               `json:"requested_at"`
	Sources        []archiveSource         `json:"sources"`
	DatabaseDigest string                  `json:"database_digest"`
	LLMBytes       int64                   `json:"llm_bytes"`
	LLMDigest      string                  `json:"llm_digest"`
	Traffic        traffic.ArchiveEstimate `json:"traffic"`
	Space          archiveSpaceEstimate    `json:"space"`
}

type archiveByteCounter struct {
	n      int64
	writer io.Writer
}

func (w *archiveByteCounter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.n += int64(n)
	return n, err
}

func archiveDatabaseDigest(snapshot *db.TaskArchiveSnapshot, llm string) (string, error) {
	counts := map[string]int64{}
	for name, count := range snapshot.DataCounts {
		if name != "traffic" {
			counts[name] = count
		}
	}
	raw, err := json.Marshal([]any{snapshot.TaskID, snapshot.ExplorationID, snapshot.Tables, counts, snapshot.AggregateStats, snapshot.SourceTaskIDs, snapshot.ExclusiveAssetIDs, snapshot.Hosts, snapshot.ExclusiveHosts, snapshot.Dependencies, llm})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Server) archiveDatabaseSnapshot(id int64, writer io.Writer) (*db.TaskArchiveSnapshot, int64, string, error) {
	h := sha256.New()
	counter := &archiveByteCounter{writer: io.MultiWriter(writer, h)}
	snapshot, err := s.m.pg.SnapshotTaskArchiveWithLLMRecords(id, counter)
	return snapshot, counter.n, hex.EncodeToString(h.Sum(nil)), err
}

// Resolve even a not-yet-created archive directory through its nearest existing
// ancestor. A symlink must not hide that the destination is inside a source.
func archiveResolvedLocation(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	probe := absolute
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			suffix, e := filepath.Rel(probe, absolute)
			if e != nil {
				return "", e
			}
			return filepath.Join(resolved, suffix), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		probe = parent
	}
}

func validateArchiveSourceSeparation(source, archiveRoot string) error {
	resolvedSource, err := archiveResolvedLocation(source)
	if err != nil {
		return err
	}
	resolvedRoot, err := archiveResolvedLocation(archiveRoot)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(resolvedSource, resolvedRoot)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return errors.New("归档目录不能位于被归档的任务或依赖目录内（包括符号链接目标）")
	}
	return nil
}

func (s *Server) collectArchiveSources(taskID, expID int64, snapshot *db.TaskArchiveSnapshot) ([]archiveSource, error) {
	var sources []archiveSource
	seen := map[string]bool{}
	add := func(relative string, owned bool) error {
		if seen[relative] {
			return nil
		}
		seen[relative] = true
		path, err := filepath.Abs(filepath.Join(s.m.dir, relative))
		if err != nil {
			return err
		}
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		if err := validateArchiveSourceSeparation(path, taskArchiveRoot(s.m.dir)); err != nil {
			return err
		}
		records, err := archiveInventory(path, false)
		if err != nil {
			return err
		}
		sources = append(sources, archiveSource{Source: path, Relative: filepath.ToSlash(filepath.Join("files", relative)), Entries: records, Owned: owned})
		return nil
	}
	if err := add(filepath.Join("tasks", strconv.FormatInt(taskID, 10)), true); err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("exp%d-", expID)
	for _, base := range []string{"transcripts", "noa", "sessions"} {
		entries, err := os.ReadDir(filepath.Join(s.m.dir, base))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), prefix) {
				if err := add(filepath.Join(base, entry.Name()), true); err != nil {
					return nil, err
				}
			}
		}
	}
	var conversations []struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(snapshot.Tables["conversations"], &conversations); err != nil {
		return nil, err
	}
	for _, conversation := range conversations {
		key := "conv-" + strconv.FormatInt(conversation.ID, 10)
		s.chatMu.Lock()
		busy := s.chatBusy[key]
		s.chatMu.Unlock()
		if busy {
			return nil, fmt.Errorf("关联会话 %d 仍在运行", conversation.ID)
		}
		for _, relative := range []string{filepath.Join("sessions", key), filepath.Join("transcripts", key+".jsonl"), filepath.Join("transcripts", key), filepath.Join("noa", "chat-"+key)} {
			if err := add(relative, false); err != nil {
				return nil, err
			}
		}
	}
	var sideSessions []struct {
		SessionKey    string `json:"session_key"`
		ExplorationID int64  `json:"exploration_id"`
		TaskID        int64  `json:"task_id"`
	}
	if err := json.Unmarshal(snapshot.Tables["side_question_sessions"], &sideSessions); err != nil {
		return nil, err
	}
	sideByKey := map[string]struct {
		ExpID int64
		Owned bool
	}{}
	for _, session := range sideSessions {
		sideByKey[session.SessionKey] = struct {
			ExpID int64
			Owned bool
		}{session.ExplorationID, session.TaskID == taskID}
	}
	var sideRequests []struct {
		ID         string `json:"id"`
		SessionKey string `json:"session_key"`
	}
	if err := json.Unmarshal(snapshot.Tables["side_question_requests"], &sideRequests); err != nil {
		return nil, err
	}
	for _, request := range sideRequests {
		if request.ID == "" || request.ID == "." || request.ID == ".." || strings.ContainsAny(request.ID, "/\\") {
			return nil, errors.New("侧会话请求 ID 无法安全映射文件")
		}
		session := sideByKey[request.SessionKey]
		if err := add(filepath.Join("transcripts", fmt.Sprintf("exp%d-btw-%s.jsonl", session.ExpID, request.ID)), session.Owned); err != nil {
			return nil, err
		}
	}
	var skills []struct {
		Skill string `json:"skill"`
		Found bool   `json:"found"`
	}
	if err := json.Unmarshal(snapshot.Tables["skill_usage"], &skills); err != nil {
		return nil, err
	}
	for _, skill := range skills {
		if !skill.Found {
			continue
		}
		if !validSkillName(skill.Skill) {
			return nil, fmt.Errorf("Skill 依赖路径非法: %s", skill.Skill)
		}
		key := "dependencies/skills/" + skill.Skill
		if seen[key] {
			continue
		}
		seen[key] = true
		if s.skillDir == "" {
			return nil, fmt.Errorf("无法定位任务使用的 Skill %s", skill.Skill)
		}
		path, err := filepath.Abs(filepath.Join(s.skillDir, skill.Skill))
		if err != nil {
			return nil, err
		}
		if err := validateArchiveSourceSeparation(path, taskArchiveRoot(s.m.dir)); err != nil {
			return nil, err
		}
		records, err := archiveInventory(path, false)
		if err != nil {
			return nil, fmt.Errorf("任务 Skill %s 缺失: %w", skill.Skill, err)
		}
		sources = append(sources, archiveSource{Source: path, Relative: key, Entries: records, Owned: false})
	}
	return sources, nil
}

func (s *Server) estimateArchive(ctx context.Context, id int64) (*archiveAdmissionPlan, *db.TaskArchiveSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	task, err := s.m.pg.GetTask(id)
	if err != nil {
		return nil, nil, err
	}
	if task == nil {
		return nil, nil, db.ErrTaskArchiveNotFound
	}
	if task.Queued || (!task.Paused && !db.IsTerminal(task.Status)) {
		return nil, nil, db.ErrTaskArchiveIneligible
	}
	snapshot, llmBytes, llmDigest, err := s.archiveDatabaseSnapshot(id, io.Discard)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range []string{"side_question_requests", "finding_retests", "finding_case_review_runs"} {
		var active []struct {
			Status string `json:"status"`
			State  string `json:"state"`
		}
		if err := json.Unmarshal(snapshot.Tables[name], &active); err != nil {
			return nil, nil, err
		}
		for _, row := range active {
			if row.Status == "running" || row.State == "queued" || row.State == "running" || row.Status == "pending" {
				return nil, nil, fmt.Errorf("关联历史 %s 仍有未完成执行，请先停止或等待结束", name)
			}
		}
	}
	plan := &archiveAdmissionPlan{TaskID: id, ExplorationID: task.ExplorationID, SnapshotAt: time.Now().UTC(), LLMBytes: llmBytes, LLMDigest: llmDigest}
	plan.DatabaseDigest, err = archiveDatabaseDigest(snapshot, llmDigest)
	if err != nil {
		return nil, nil, err
	}
	plan.Sources, err = s.collectArchiveSources(id, task.ExplorationID, snapshot)
	if err != nil {
		return nil, nil, err
	}
	root := taskArchiveRoot(s.m.dir)
	if err := os.MkdirAll(root, archiveDirMode); err != nil {
		return nil, nil, err
	}
	query := s.archiveSpaceQuery
	if query == nil {
		query = archiveDiskSpace
	}
	available, block, err := query(root)
	if err != nil {
		return nil, nil, fmt.Errorf("读取归档目标盘剩余空间: %w", err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return nil, nil, err
	}
	bytes := uint64(llmBytes) + uint64(len(raw))*2
	entries := int64(len(snapshot.Tables) + 8)
	for _, source := range plan.Sources {
		for _, entry := range source.Entries {
			if entry.Size < 0 {
				return nil, nil, errors.New("负数文件大小")
			}
			if uint64(entry.Size) > ^uint64(0)-bytes {
				return nil, nil, errors.New("归档大小溢出")
			}
			bytes += uint64(entry.Size)
			metadata, err := json.Marshal(entry)
			if err != nil {
				return nil, nil, err
			}
			bytes += uint64(len(metadata))*2 + 256 // source plan + verified inventory, including hashes
			entries++
		}
	}
	if entries > maxArchiveFiles {
		return nil, nil, errors.New("任务文件数超过归档安全上限，未进行归档")
	}
	if s.m.traffic != nil {
		plan.Traffic, err = s.m.traffic.SnapshotHostsBefore(snapshot.Hosts, plan.SnapshotAt, "")
		if err != nil {
			return nil, nil, err
		}
		bytes += uint64(plan.Traffic.Bytes)
		entries += plan.Traffic.Files
	}
	evidence, err := db.ArchiveEvidenceSnapshots(snapshot)
	if err != nil {
		return nil, nil, err
	}
	unique := map[string]bool{}
	for _, body := range evidence {
		for _, side := range []string{"request", "response"} {
			hash := body.ReqHash
			if side == "response" {
				hash = body.RespHash
			}
			if unique[hash] {
				continue
			}
			unique[hash] = true
			f, length, err := s.evidenceStore().OpenBody(body, side)
			if err != nil {
				return nil, nil, err
			}
			f.Close()
			if length < 0 {
				return nil, nil, errors.New("证据正文长度无效")
			}
			bytes += uint64(length)
			entries++
		}
	}
	required, err := archiveRequiredSpace(bytes, entries, block)
	if err != nil {
		return nil, nil, err
	}
	plan.Space = archiveSpaceEstimate{EstimatedBytes: bytes, RequiredBytes: required, AvailableBytes: available, FileCount: entries, Sufficient: available >= required, SnapshotAt: plan.SnapshotAt}
	return plan, snapshot, nil
}

func checkArchiveCapacity(estimate archiveSpaceEstimate) error {
	if !estimate.Sufficient {
		return &archiveCapacityError{Estimate: estimate}
	}
	return nil
}

type archiveCapacityError struct {
	Estimate  archiveSpaceEstimate
	Operation string
}

func (e *archiveCapacityError) Error() string {
	operation := e.Operation
	if operation == "" {
		operation = "归档"
	}
	return fmt.Sprintf("%s空间不足：预计数据 %d 字节，含文件分配和安全余量需 %d 字节，目标盘可用 %d 字节；原数据保留", operation, e.Estimate.EstimatedBytes, e.Estimate.RequiredBytes, e.Estimate.AvailableBytes)
}

func (s *Server) estimateTaskArchives(w http.ResponseWriter, r *http.Request) {
	var input archiveBatchRequest
	if err := decode(r, &input); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ids := normalizeBatchTaskIDs(input.TaskIDs)
	if len(ids) == 0 || len(ids) > 100 {
		writeErr(w, 400, "请选择 1 到 100 个任务")
		return
	}
	var total archiveSpaceEstimate
	var available uint64
	first := true
	for _, id := range ids {
		if !id.valid {
			writeErr(w, 400, "任务 id 无效")
			return
		}
		numeric, _ := strconv.ParseInt(id.id, 10, 64)
		plan, _, err := s.estimateArchive(r.Context(), numeric)
		if err != nil {
			writeArchiveError(w, err)
			return
		}
		total.EstimatedBytes += plan.Space.EstimatedBytes
		total.FileCount += plan.Space.FileCount
		processes, writers, err := s.archiveProcessState(numeric, plan.Sources)
		if err != nil {
			writeArchiveError(w, err)
			return
		}
		total.Writers = append(total.Writers, writers...)
		total.BackgroundProcesses = append(total.BackgroundProcesses, processes...)
		if first || plan.Space.AvailableBytes < available {
			available = plan.Space.AvailableBytes
			first = false
		}
	}
	query := s.archiveSpaceQuery
	if query == nil {
		query = archiveDiskSpace
	}
	latestAvailable, block, err := query(taskArchiveRoot(s.m.dir))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if latestAvailable < available {
		available = latestAvailable
	}
	total.RequiredBytes, err = archiveRequiredSpace(total.EstimatedBytes, total.FileCount, block)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	total.AvailableBytes = available
	total.Sufficient = available >= total.RequiredBytes
	total.SnapshotAt = time.Now().UTC()
	writeJSON(w, 200, total)
}

func archiveSourceRoots(sources []archiveSource) []string {
	roots := make([]string, 0, len(sources))
	for _, source := range sources {
		roots = append(roots, source.Source)
	}
	return roots
}

func copyArchiveSource(ctx context.Context, source archiveSource, destination string) error {
	target := filepath.Join(destination, filepath.FromSlash(source.Relative))
	if len(source.Entries) == 1 && source.Entries[0].Kind != "directory" {
		record := source.Entries[0]
		record.Path = filepath.Base(source.Source)
		if filepath.Base(target) != record.Path {
			return errors.New("归档源文件名不匹配")
		}
		return copyArchiveTree(ctx, filepath.Dir(source.Source), filepath.Dir(target), []archiveFileRecord{record})
	}
	return copyArchiveTree(ctx, source.Source, target, source.Entries)
}

// Capacity is checked before execution is interrupted and again after obtaining
// the task write barrier. The handler captures the accepted snapshot itself;
// a FIFO worker must not replace it with whatever data exists minutes later.
func (s *Server) prepareTaskArchive(ctx context.Context, id int64) (item *db.TaskArchive, err error) {
	requestedAt := time.Now().UTC()
	servicesStopped := false
	defer func() {
		if err != nil && servicesStopped {
			err = fmt.Errorf("归档未完成，原数据保留；已停止的后台服务不会自动重启: %w", err)
		}
	}()
	s.archivePrepareMu.Lock()
	defer s.archivePrepareMu.Unlock()
	previous, err := s.m.pg.GetTaskArchiveByTask(id)
	if err != nil {
		return nil, err
	}
	if previous != nil {
		if err = recoverTaskArchiveStages(s.m.dir, s.m.pg, previous.ID); err != nil {
			return nil, err
		}
	}
	plan, _, err := s.estimateArchive(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkArchiveCapacity(plan.Space); err != nil {
		return nil, err
	}
	taskID := strconv.FormatInt(id, 10)
	if !s.beginTaskDelete(taskID) {
		return nil, errors.New("任务已有归档或删除操作")
	}
	defer s.abortTaskDelete(taskID)
	// Running sessions are rejected rather than cancelled after the cutoff: their
	// final writes must be visible before a user can request a complete snapshot.
	s.chatMu.Lock()
	busy := s.chatBusy[taskID]
	s.chatMu.Unlock()
	if busy || s.engine.inflightCount(taskID) != 0 {
		return nil, errors.New("任务仍有运行中的 Agent 或对话，请等待结束后重试归档")
	}
	processes, writers, processErr := s.archiveProcessState(id, plan.Sources)
	if processErr != nil {
		return nil, processErr
	}
	if unknown := unknownArchiveWriters(writers); len(unknown) > 0 {
		return nil, &archiveWriterConflict{Writers: writers}
	}
	// Capacity was checked before stopping anything. Only persisted, task-owned
	// services can be stopped; legacy/foreign writers require operator action.
	// Re-estimate afterwards so shutdown logs and state are in the snapshot.
	if s.processes != nil {
		servicesStopped = len(processes) > 0
		if err = s.processes.Stop(ctx, id); err != nil {
			return nil, &archiveServiceConflict{Err: err}
		}
	}
	plan, snapshot, err := s.estimateArchive(ctx, id)
	if err != nil {
		return nil, err
	}
	plan.RequestedAt = requestedAt
	if err = checkArchiveCapacity(plan.Space); err != nil {
		return nil, err
	}
	if err = archiveCheckWriters(archiveSourceRoots(plan.Sources)); err != nil {
		return nil, err
	}
	item, err = s.m.pg.QueueTaskArchive(id, &db.ArchivePreparation{EstimatedBytes: int64(plan.Space.EstimatedBytes), RequiredBytes: int64(plan.Space.RequiredBytes), AvailableBytes: int64(plan.Space.AvailableBytes), SnapshotAt: plan.SnapshotAt})
	if err != nil {
		return nil, err
	}
	preparationID := item.ID
	defer func() {
		if err != nil {
			err = errors.Join(err, s.m.pg.FailTaskArchivePreparation(preparationID, err))
		}
	}()
	if err = s.m.pg.UpdateTaskArchiveProgress(preparationID, "snapshot", 5); err != nil {
		return nil, err
	}
	parent := filepath.Join(taskArchiveRoot(s.m.dir), ".preparing")
	if err = os.MkdirAll(parent, archiveDirMode); err != nil {
		return nil, err
	}
	prepared, err := os.MkdirTemp(parent, "snapshot-")
	if err != nil {
		return nil, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = removeManagedArchive(s.m.dir, prepared)
		}
	}()
	if err = os.MkdirAll(filepath.Join(prepared, "database"), archiveDirMode); err != nil {
		return nil, err
	}
	stream, err := os.OpenFile(filepath.Join(prepared, db.TaskArchiveLLMRecordsPath), os.O_CREATE|os.O_EXCL|os.O_WRONLY, archiveFileMode)
	if err != nil {
		return nil, err
	}
	frozen, _, digest, snapshotErr := s.archiveDatabaseSnapshot(id, stream)
	if err = errors.Join(snapshotErr, stream.Sync(), stream.Close()); err != nil {
		return nil, err
	}
	current, err := archiveDatabaseDigest(frozen, digest)
	if err != nil {
		return nil, err
	}
	if current != plan.DatabaseDigest {
		return nil, errors.New("任务数据库在受理归档后发生变化，原数据保留，请重试")
	}
	frozen.SnapshotAt = plan.SnapshotAt
	frozen.LLMDigest = digest
	snapshot = frozen
	for i, source := range plan.Sources {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		copySource := s.archiveCopySource
		if copySource == nil {
			copySource = copyArchiveSource
		}
		if err = copySource(ctx, source, prepared); err != nil {
			return nil, err
		}
		actual, inventoryErr := archiveInventory(source.Source, true)
		if inventoryErr != nil {
			return nil, inventoryErr
		}
		if err = sameArchiveInventory(source.Entries, actual, false); err != nil {
			return nil, fmt.Errorf("%s: %w", source.Source, err)
		}
		copied, copyErr := archiveInventory(filepath.Join(prepared, filepath.FromSlash(source.Relative)), true)
		if copyErr != nil {
			return nil, copyErr
		}
		if err = sameArchiveInventory(actual, copied, true); err != nil {
			return nil, fmt.Errorf("归档副本不完整 %s: %w", source.Source, err)
		}
		plan.Sources[i].Entries = actual
	}
	if s.m.traffic != nil {
		captured, captureErr := s.m.traffic.SnapshotHostsBefore(snapshot.Hosts, plan.SnapshotAt, filepath.Join(prepared, "traffic"))
		if captureErr != nil {
			return nil, captureErr
		}
		if captured != plan.Traffic {
			return nil, errors.New("受理归档后的流量快照发生变化，原数据保留")
		}
		snapshot.DataCounts["traffic"] = captured.Exchanges
	}
	evidence, err := db.ArchiveEvidenceSnapshots(snapshot)
	if err != nil {
		return nil, err
	}
	if err = s.evidenceStore().CopySnapshots(ctx, evidence, filepath.Join(prepared, "evidence")); err != nil {
		return nil, err
	}
	for name, rows := range snapshot.Tables {
		if strings.ContainsAny(name, "/\\.") {
			return nil, errors.New("非法快照表名")
		}
		if err = writeArchiveFile(filepath.Join(prepared, "database", name+".json"), rows); err != nil {
			return nil, err
		}
	}
	dependencies, err := json.Marshal(snapshot.Dependencies)
	if err != nil {
		return nil, err
	}
	if err = writeArchiveFile(filepath.Join(prepared, "dependencies.json"), dependencies); err != nil {
		return nil, err
	}
	manifest, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	if err = writeArchiveFile(filepath.Join(prepared, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	planRaw, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	if err = writeArchiveFile(filepath.Join(prepared, "source-plan.json"), planRaw); err != nil {
		return nil, err
	}
	summary, err := json.MarshalIndent(map[string]any{"task_id": id, "requested_at": plan.RequestedAt, "snapshot_at": plan.SnapshotAt, "program_version": BuildVersion, "data_counts": snapshot.DataCounts, "aggregate_stats": snapshot.AggregateStats, "space": plan.Space, "scope": "task data and associated records; global authentication secrets are not part of a task archive", "traffic_hot_copy_retained": true}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err = writeArchiveFile(filepath.Join(prepared, "summary.json"), summary); err != nil {
		return nil, err
	}
	if err = writeArchiveFile(filepath.Join(prepared, "archive.log"), []byte(fmt.Sprintf("snapshot_at=%s task_id=%d phase=verified_snapshot sources=%d\n", plan.SnapshotAt.Format(time.RFC3339Nano), id, len(plan.Sources)))); err != nil {
		return nil, err
	}
	if err = archiveCheckWriters(archiveSourceRoots(plan.Sources)); err != nil {
		return nil, err
	}
	for _, source := range plan.Sources {
		actual, e := archiveInventory(source.Source, true)
		if e != nil {
			return nil, e
		}
		if e = sameArchiveInventory(source.Entries, actual, true); e != nil {
			return nil, e
		}
	}
	checksum, _, err := sealDirectoryArchive(prepared)
	if err != nil {
		return nil, err
	}
	if err = verifyDirectoryArchive(prepared, checksum); err != nil {
		return nil, err
	}
	complete = true // Retain a verified copy even if publishing its DB pointer fails.
	finalParent := filepath.Join(taskArchiveRoot(s.m.dir), "task-"+taskID)
	if err = os.MkdirAll(finalParent, archiveDirMode); err != nil {
		return nil, err
	}
	final := filepath.Join(finalParent, plan.SnapshotAt.Format("20060102T150405Z")+"-"+filepath.Base(prepared))
	if err = os.Rename(prepared, final); err != nil {
		return nil, err
	}
	prepared = final
	if err = syncArchiveAncestors(finalParent); err != nil {
		return nil, err
	}
	item, err = s.m.pg.PublishTaskArchivePreparation(preparationID, prepared, checksum)
	if err != nil {
		return nil, err
	}
	return item, nil
}
