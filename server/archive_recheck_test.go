package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
)

func TestDirectoryArchiveEstimateUsesLatestFreeSpace(t *testing.T) {
	s, task, _ := directoryArchiveFixture(t)
	checks := 0
	s.archiveSpaceQuery = func(string) (uint64, uint64, error) {
		checks++
		if checks > 1 {
			return 0, 4096, nil
		}
		return 1 << 40, 4096, nil
	}
	w := httptest.NewRecorder()
	s.estimateTaskArchives(w, httptest.NewRequest("POST", "/api/tasks/archive/estimate", strings.NewReader(`{"task_ids":["`+task.ID+`"]}`)))
	var estimate archiveSpaceEstimate
	if err := json.Unmarshal(w.Body.Bytes(), &estimate); err != nil || w.Code != 200 || estimate.Sufficient || estimate.AvailableBytes != 0 {
		t.Fatalf("stale free space used: %d %s %v", w.Code, w.Body.String(), err)
	}
}

func TestDirectoryArchiveActualCopyDiskFullKeepsOriginalAndCleansPartialSnapshot(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	s.archiveCopySource = func(_ context.Context, _ archiveSource, destination string) error {
		file := filepath.Join(destination, "partial-copy")
		if err := os.WriteFile(file, []byte("incomplete"), 0600); err != nil {
			t.Fatal(err)
		}
		return &os.PathError{Op: "write", Path: file, Err: syscall.ENOSPC}
	}
	if _, err := s.prepareTaskArchive(t.Context(), id); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("copy failure lost: %v", err)
	}
	job, err := s.m.pg.GetTaskArchiveByTask(id)
	if err != nil || job == nil || job.State != db.ArchiveFailed {
		t.Fatalf("copy failure state=%+v %v", job, err)
	}
	if raw, err := os.ReadFile(filepath.Join(s.m.dir, "tasks", task.ID, "workspace", "binary.bin")); err != nil || !bytes.Equal(raw, []byte{0, 1, 2, 255, 0, 10}) {
		t.Fatalf("copy failure changed original: %v %v", raw, err)
	}
	entries, err := os.ReadDir(filepath.Join(taskArchiveRoot(s.m.dir), ".preparing"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial snapshot not removed: %d %v", len(entries), err)
	}
	if s.engine.IsDeleting(task.ID) {
		t.Fatal("failed capture retained write barrier")
	}
}

func TestDirectoryArchiveConflictingUsageDimensionsAreNotDeleted(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	other, err := s.m.CreateTask("other usage owner", "separate task", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	otherID, _ := strconv.ParseInt(other.ID, 10, 64)
	t.Cleanup(func() { _ = s.m.pg.DeleteTask(otherID) })
	var usageID int64
	if err = s.m.pg.QueryRow(`INSERT INTO tool_usage(tool_key,task_id,exploration_id) VALUES('ambiguous-usage',$1,$2) RETURNING id`, otherID, task.Store.ID()).Scan(&usageID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.prepareTaskArchive(t.Context(), id); err == nil || !strings.Contains(err.Error(), "归属不一致") {
		t.Fatalf("ambiguous usage accepted: %v", err)
	}
	var count int
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM tool_usage WHERE id=$1`, usageID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("other task usage removed: %d %v", count, err)
	}
}

func TestDirectoryArchiveBoundEvidenceAndTrafficSurviveOriginalStoreLoss(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	var err error
	s.m.traffic, err = traffic.Open(filepath.Join(s.m.dir, "traffic"), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const exchangeID = "directory-evidence-fixture"
	if _, err = s.m.traffic.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path) VALUES(?,1,'fixture.local','POST','/test','https://fixture.local/test',200,'application/octet-stream',2,4,'')`, exchangeID); err != nil {
		t.Fatal(err)
	}
	body := []byte{0, 255, 128, 10}
	if _, err = s.m.traffic.DB().Exec(`INSERT INTO exchange_bodies(id,req_head,req_body,resp_head,resp_body) VALUES(?,'POST /test',?,'HTTP 200',?)`, exchangeID, []byte("{}"), body); err != nil {
		t.Fatal(err)
	}
	finding, err := s.evidenceStore().Record(t.Context(), db.RecordFindingInput{TaskID: id, ExplorationID: task.Store.ID(), Worker: "test", VulnClass: "TEST", Name: "binary evidence", Severity: "low", Summary: "original evidence"}, []db.TrafficRef{{TrafficID: exchangeID}})
	if err != nil {
		t.Fatal(err)
	}
	var assetID int64
	if err = s.m.pg.QueryRow(`INSERT INTO assets(type,domain,task_ids) VALUES('root_domain','fixture.local',ARRAY[$1]::bigint[]) RETURNING id`, id).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM assets WHERE id=$1`, assetID) })
	job, err := s.prepareTaskArchive(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	job, err = s.m.pg.GetTaskArchive(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Remove only this isolated fixture's hot stores. Restoration must use its
	// own complete archive, including bytes no longer available in either store.
	if _, err = s.m.traffic.DeleteHost("fixture.local"); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(filepath.Join(s.m.dir, "evidence")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.m.pg.QueueTaskArchiveRestore(job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	bindings, err := s.m.pg.GetFindingTraffic(t.Context(), finding.FindingID)
	if err != nil || len(bindings.Bindings) != 1 {
		t.Fatalf("restored bindings=%+v %v", bindings, err)
	}
	f, length, err := s.evidenceStore().OpenBody(bindings.Bindings[0].Snapshot, "response")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, length)
	_, err = f.Read(got)
	closeErr := f.Close()
	if err != nil || closeErr != nil || !bytes.Equal(got, body) {
		t.Fatalf("restored evidence=%v err=%v close=%v", got, err, closeErr)
	}
	if _, _, err = s.m.traffic.Get(exchangeID); err != nil {
		t.Fatalf("raw traffic omitted: %v", err)
	}
	if err = verifyDirectoryArchive(job.ArchivePath, job.SHA256); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryArchiveRestoreChecksDataMountBeforeCopy(t *testing.T) {
	s, _, id := directoryArchiveFixture(t)
	job, err := s.prepareTaskArchive(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	job, err = s.m.pg.GetTaskArchive(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.archiveSpaceQuery = func(path string) (uint64, uint64, error) {
		if path != s.m.dir {
			t.Fatalf("checked wrong restore mount: %s", path)
		}
		return 0, 4096, nil
	}
	if _, err = s.m.pg.QueueTaskArchiveRestore(job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err == nil || !strings.Contains(err.Error(), "恢复空间不足") {
		t.Fatalf("insufficient restore accepted: %v", err)
	}
	if _, err = os.Stat(filepath.Join(s.m.dir, "archives", "tasks", ".restore")); !os.IsNotExist(err) {
		t.Fatalf("created clone before capacity check: %v", err)
	}
	if err = verifyDirectoryArchive(job.ArchivePath, job.SHA256); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryArchiveRecoveryAfterCommitAndRestoreQueued(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	job, err := s.prepareTaskArchive(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	job, err = s.m.pg.GetTaskArchive(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	stageRoot := filepath.Join(s.m.dir, "archives", "tasks", ".staging", strconv.FormatInt(job.ID, 10))
	entries, err := archiveInventory(filepath.Join(job.ArchivePath, "files"), true)
	if err != nil {
		t.Fatal(err)
	}
	if err = copyArchiveTree(t.Context(), filepath.Join(job.ArchivePath, "files"), filepath.Join(stageRoot, "payload", "files"), entries); err != nil {
		t.Fatal(err)
	}
	journal := archiveStageJournal{ArchiveID: job.ID, TaskID: task.ID, Moves: []archiveFileMove{{Source: filepath.Join(s.m.dir, "tasks", task.ID), Relative: filepath.Join("files", "tasks", task.ID)}}}
	if err = writeArchiveJournal(filepath.Join(stageRoot, "journal.json"), journal); err != nil {
		t.Fatal(err)
	}
	if _, err = s.m.pg.QueueTaskArchiveRestore(job.ID); err != nil {
		t.Fatal(err)
	}
	if err = recoverTaskArchiveStages(s.m.dir, s.m.pg); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(s.m.dir, "tasks", task.ID)); !os.IsNotExist(err) {
		t.Fatalf("committed archive original wrongly resurrected: %v", err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	if row, err := s.m.pg.GetTaskArchive(job.ID); err != nil || row != nil {
		t.Fatalf("restore incomplete: %+v %v", row, err)
	}
}

func TestDirectoryArchiveRestoreRollbackConflictRetainsJournalAndCanRetry(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	job, err := s.prepareTaskArchive(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	job, err = s.m.pg.GetTaskArchive(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(s.m.dir, "archives", "tasks", ".restore", strconv.FormatInt(job.ID, 10)+"-interrupted")
	source := filepath.Join(work, "files", "tasks", task.ID)
	if err = os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "retained"), []byte("copy"), 0600); err != nil {
		t.Fatal(err)
	}
	stage, err := installTaskArchiveFiles(s.m.dir, work, task.ID, job.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	} // Simulate a rollback destination conflict.
	if err = stage.rollback(); err == nil {
		t.Fatal("rollback conflict ignored")
	}
	if err = cleanupTaskArchiveRestoreWork(work); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(work, "restore-journal.json")); err != nil {
		t.Fatalf("recovery journal destroyed: %v", err)
	}
	if err = os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if _, err = s.m.pg.QueueTaskArchiveRestore(job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatalf("same-process retry failed: %v", err)
	}
	if row, err := s.m.pg.GetTaskArchive(job.ID); err != nil || row != nil {
		t.Fatalf("restore incomplete: %+v %v", row, err)
	}
	if err = verifyDirectoryArchive(job.ArchivePath, job.SHA256); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryArchiveRetryRecoversOriginalStagingBeforeResnapshot(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	job, err := s.prepareTaskArchive(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.m.pg.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = stageTaskArchiveFiles(s.m.dir, job.ID, task.ID, task.Store.ID()); err != nil {
		t.Fatal(err)
	}
	if err = s.m.pg.FailTaskArchiveJob(job.ID, db.Archiving, os.ErrPermission); err != nil {
		t.Fatal(err)
	}
	retried, err := s.prepareTaskArchive(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(retried.PreparedPath, "files", "tasks", task.ID, "workspace", "binary.bin")
	if raw, err := os.ReadFile(file); err != nil || string(raw) != string([]byte{0, 1, 2, 255, 0, 10}) {
		t.Fatalf("retry omitted staged original: %v %v", raw, err)
	}
}

func TestDirectoryArchiveAssociatedConversationSideHistoryIsComplete(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	var conv, approval, node, finding int64
	if err := s.m.pg.QueryRow(`INSERT INTO conversations(title,agent_key) VALUES('associated fixture','retester') RETURNING id`).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	key := "conversation-side-" + task.ID
	requestID := "side-request-" + task.ID
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, conv)
		_, _ = s.m.pg.Exec(`DELETE FROM llm_records WHERE session_id=$1`, "exp0-btw-"+requestID)
	})
	if err := s.m.pg.QueryRow(`INSERT INTO exploration_nodes(exploration_id,kind,state,payload) VALUES($1,'finding','confirmed','{}') RETURNING id`, task.Store.ID()).Scan(&node); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.QueryRow(`INSERT INTO findings(task_id,node_id,name) VALUES($1,$2,'retest fixture') RETURNING id`, id, node).Scan(&finding); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO finding_retests(finding_id,conversation_id,status,snapshot) VALUES($1,$2,'completed','{}')`, finding, conv); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.QueryRow(`INSERT INTO intercept_pending(conversation_id,tool_name,status,decision_source) VALUES($1,'fixture','allowed','manual') RETURNING id`, conv).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO side_question_sessions(session_key,conversation_id,run_id,version,snapshot) VALUES($1,$2,1,1,'{}')`, key, conv); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO side_question_requests(id,session_key,generation,client_id,question,answer,status,model,snapshot_at) VALUES($1,$2,0,'client','question','answer','completed','{}',now())`, requestID, key); err != nil {
		t.Fatal(err)
	}
	transcript := "exp0-btw-" + requestID + ".jsonl"
	if err := os.WriteFile(filepath.Join(s.m.dir, "transcripts", transcript), []byte("associated side transcript"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO llm_records(session_id,raw_response) VALUES($1,'side raw response')`, strings.TrimSuffix(transcript, ".jsonl")); err != nil {
		t.Fatal(err)
	}
	job, err := s.prepareTaskArchive(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot db.TaskArchiveSnapshot
	raw, err := os.ReadFile(filepath.Join(job.PreparedPath, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"side_question_sessions", "side_question_requests", "intercept_pending", "llm_records"} {
		if snapshot.DataCounts[table] != 1 {
			t.Fatalf("associated %s omitted: %+v", table, snapshot.DataCounts)
		}
	}
	if raw, err = os.ReadFile(filepath.Join(job.PreparedPath, "files", "transcripts", transcript)); err != nil || string(raw) != "associated side transcript" {
		t.Fatalf("side transcript omitted: %s %v", raw, err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.m.pg.QueueTaskArchiveRestore(job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatalf("shared history restoration conflicted: %v", err)
	}
	var count int
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM side_question_requests WHERE id=$1 AND answer='answer'`, requestID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("side history missing/duplicated: %d %v", count, err)
	}
}

func TestDirectoryArchiveActiveSideQuestionPreventsSnapshot(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	key := "active-side-" + task.ID
	if _, err := s.m.pg.Exec(`INSERT INTO side_question_sessions(session_key,task_id,exploration_id,run_id,version,snapshot) VALUES($1,$2,$3,1,1,'{}')`, key, id, task.Store.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO side_question_requests(id,session_key,generation,client_id,question,status,model,snapshot_at) VALUES($1,$1,0,'client','question','running','{}',now())`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.prepareTaskArchive(t.Context(), id); err == nil || !strings.Contains(err.Error(), "未完成执行") {
		t.Fatalf("active side execution accepted: %v", err)
	}
	if job, err := s.m.pg.GetTaskArchiveByTask(id); err != nil || job != nil {
		t.Fatalf("active question queued: %+v %v", job, err)
	}
}

func TestDirectoryArchiveRecoveryPreservesPayloadWithMissingJournal(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	job, err := s.m.pg.QueueTaskArchive(id, &db.ArchivePreparation{})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(s.m.dir, "archives", "tasks", ".staging", strconv.FormatInt(job.ID, 10))
	file := filepath.Join(root, "payload", "files", "tasks", task.ID, "only-original")
	if err = os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, []byte("only copy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = recoverTaskArchiveStages(s.m.dir, s.m.pg, job.ID); err == nil {
		t.Fatal("unknown payload silently removed")
	}
	if raw, err := os.ReadFile(file); err != nil || string(raw) != "only copy" {
		t.Fatalf("unknown payload destroyed: %s %v", raw, err)
	}
}

func TestDirectoryArchiveLegacyUntaggedSessionRecordsArePreserved(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	session := "exp" + strconv.FormatInt(task.Store.ID(), 10) + "-main"
	if _, err := s.m.pg.Exec(`INSERT INTO llm_records(session_id,raw_response) VALUES($1,'legacy raw bytes')`, session); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO skill_usage(skill,session_id,found) VALUES('missing-legacy', $1,false)`, session); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO tool_usage(tool_key,session_id) VALUES('legacy-tool',$1)`, session); err != nil {
		t.Fatal(err)
	}
	job, err := s.prepareTaskArchive(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	archived, err := s.m.pg.GetTaskArchive(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stats struct {
		Tools             map[string]int `json:"tools"`
		MissingSkillStats []struct {
			Skill string `json:"skill"`
			Calls int    `json:"calls"`
		} `json:"missing_skill_stats"`
	}
	if err = json.Unmarshal(archived.AggregateStats, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Tools["legacy-tool"] != 1 || len(stats.MissingSkillStats) != 1 || stats.MissingSkillStats[0].Calls != 1 {
		t.Fatalf("legacy usage disappeared from archived totals: %s", archived.AggregateStats)
	}
	var count int
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM llm_records WHERE session_id=$1`, session).Scan(&count); err != nil || count != 0 {
		t.Fatalf("legacy rows not compacted: %d %v", count, err)
	}
	if _, err = s.m.pg.QueueTaskArchiveRestore(job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`SELECT count(*) FROM llm_records WHERE session_id=$1 AND raw_response='legacy raw bytes'`, `SELECT count(*) FROM skill_usage WHERE session_id=$1 AND skill='missing-legacy'`, `SELECT count(*) FROM tool_usage WHERE session_id=$1 AND tool_key='legacy-tool'`} {
		if err = s.m.pg.QueryRow(query, session).Scan(&count); err != nil || count != 1 {
			t.Fatalf("legacy history lost: %d %v", count, err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM llm_records WHERE session_id=$1`, session)
		_, _ = s.m.pg.Exec(`DELETE FROM skill_usage WHERE session_id=$1`, session)
		_, _ = s.m.pg.Exec(`DELETE FROM tool_usage WHERE session_id=$1`, session)
	})
}

func TestDirectoryArchiveOwnedRecordConflictCannotBeSilentlySkipped(t *testing.T) {
	for _, table := range []string{"llm_records", "skill_usage"} {
		t.Run(table, func(t *testing.T) {
			s, task, id := directoryArchiveFixture(t)
			var record int64
			var err error
			if table == "llm_records" {
				err = s.m.pg.QueryRow(`INSERT INTO llm_records(task_id,raw_response) VALUES($1,'archive original') RETURNING id`, task.ID).Scan(&record)
			} else {
				err = s.m.pg.QueryRow(`INSERT INTO skill_usage(task_id,skill,found) VALUES($1,'archive-original',false) RETURNING id`, id).Scan(&record)
			}
			if err != nil {
				t.Fatal(err)
			}
			job, err := s.prepareTaskArchive(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.runOneTaskArchiveJob(); err != nil {
				t.Fatal(err)
			}
			if table == "llm_records" {
				_, err = s.m.pg.Exec(`INSERT INTO llm_records(id,raw_response) VALUES($1,'conflicting current')`, record)
			} else {
				_, err = s.m.pg.Exec(`INSERT INTO skill_usage(id,skill,found) VALUES($1,'conflicting-current',false)`, record)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM `+table+` WHERE id=$1`, record) })
			if _, err = s.m.pg.QueueTaskArchiveRestore(job.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.runOneTaskArchiveJob(); err == nil {
				t.Fatal("owned record conflict silently skipped")
			}
			if _, err = os.Stat(filepath.Join(s.m.dir, "tasks", task.ID)); !os.IsNotExist(err) {
				t.Fatalf("failed DB restore left installed task tree: %v", err)
			}
			if row, err := s.m.pg.GetTaskArchive(job.ID); err != nil || row.State != db.RestoreFailed {
				t.Fatalf("restore conflict state=%+v %v", row, err)
			}
			if _, err = s.m.pg.Exec(`DELETE FROM `+table+` WHERE id=$1`, record); err != nil {
				t.Fatal(err)
			}
			if _, err = s.m.pg.QueueTaskArchiveRestore(job.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.runOneTaskArchiveJob(); err != nil {
				t.Fatalf("retry after removing conflict failed: %v", err)
			}
		})
	}
}

func TestDirectoryArchiveRecoveryKeepsOriginalWhenColdCopyIsDamaged(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	job, err := s.prepareTaskArchive(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	job, err = s.m.pg.GetTaskArchive(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(s.m.dir, "archives", "tasks", ".staging", strconv.FormatInt(job.ID, 10))
	entries, err := archiveInventory(filepath.Join(job.ArchivePath, "files"), true)
	if err != nil {
		t.Fatal(err)
	}
	if err = copyArchiveTree(t.Context(), filepath.Join(job.ArchivePath, "files"), filepath.Join(root, "payload", "files"), entries); err != nil {
		t.Fatal(err)
	}
	journal := archiveStageJournal{ArchiveID: job.ID, TaskID: task.ID, Moves: []archiveFileMove{{Source: filepath.Join(s.m.dir, "tasks", task.ID), Relative: filepath.Join("files", "tasks", task.ID)}}}
	if err = writeArchiveJournal(filepath.Join(root, "journal.json"), journal); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(job.ArchivePath, "files", "tasks", task.ID, "workspace", "binary.bin"), []byte("damaged cold copy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = recoverTaskArchiveStages(s.m.dir, s.m.pg, job.ID); err == nil {
		t.Fatal("damaged cold copy allowed discarding original")
	}
	if raw, err := os.ReadFile(filepath.Join(root, "payload", "files", "tasks", task.ID, "workspace", "binary.bin")); err != nil || string(raw) != string([]byte{0, 1, 2, 255, 0, 10}) {
		t.Fatalf("last original destroyed: %v %v", raw, err)
	}
}

func TestDirectoryArchiveCommittedRestoreRepairsFilesBeforeCleaningJournal(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	job, err := s.prepareTaskArchive(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	job, err = s.m.pg.GetTaskArchive(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.m.pg.QueueTaskArchiveRestore(job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(s.m.dir, "archives", "tasks", ".restore", strconv.FormatInt(job.ID, 10)+"-committed")
	source := filepath.Join(work, "files", "tasks", task.ID)
	destination := filepath.Join(s.m.dir, "tasks", task.ID)
	if err = os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(destination, source); err != nil {
		t.Fatal(err)
	}
	journal := archiveRestoreJournal{ArchiveID: job.ID, TaskID: id, Moves: []restoredArchivePath{{Source: source, Destination: destination}}}
	if err = writeArchiveJournal(filepath.Join(work, "restore-journal.json"), journal); err != nil {
		t.Fatal(err)
	}
	if err = recoverTaskArchiveRestoreStages(s.m.dir, s.m.pg, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(destination, "workspace", "binary.bin")); err != nil {
		t.Fatalf("committed task files not repaired: %v", err)
	}
}
