package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func directoryArchiveFixture(t *testing.T) (*Server, *Task, int64) {
	t.Helper()
	s := concurrencyTestServer(t)
	t.Setenv("ARTEX_ARCHIVE_DIR", filepath.Join(t.TempDir(), "separate-archive-mount"))
	task, err := s.m.CreateTask("complete <task> & evidence", "archive without omissions", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	if err := s.m.pg.SetPaused(id, true); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(s.m.dir, "tasks", task.ID, "workspace", "binary.bin")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte{0, 1, 2, 255, 0, 10}, 0600); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{filepath.Join("transcripts", "exp"+strconv.FormatInt(task.Store.ID(), 10)+"-main-s3.jsonl"), filepath.Join("noa", "exp"+strconv.FormatInt(task.Store.ID(), 10)+"-planner", "summary.json")} {
		path := filepath.Join(s.m.dir, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("prior task data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM task_archives WHERE task_id=$1`, id)
		_ = s.m.pg.DeleteTask(id)
	})
	return s, task, id
}

func TestDirectoryArchiveSpaceBlocksWithoutQueueOrMutation(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	s.archiveSpaceQuery = func(string) (uint64, uint64, error) { return 0, 4096, nil }
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/tasks/"+task.ID+"/archive", nil)
	r.SetPathValue("id", task.ID)
	s.queueTaskArchive(w, r)
	if w.Code != http.StatusInsufficientStorage || !strings.Contains(w.Body.String(), "available_bytes") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	job, err := s.m.pg.GetTaskArchiveByTask(id)
	if err != nil || job != nil {
		t.Fatalf("space failure queued work: %+v %v", job, err)
	}
	if s.engine.IsDeleting(task.ID) {
		t.Fatal("insufficient space changed execution barrier")
	}
	if _, err := os.Stat(filepath.Join(s.m.dir, "tasks", task.ID, "workspace", "binary.bin")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(taskArchiveRoot(s.m.dir), ".preparing")); !os.IsNotExist(err) {
		t.Fatal("payload created before capacity check")
	}
}

func TestDirectoryArchiveCompleteRoundTripAndRetention(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	var findingIDs []int64
	for range 2 {
		var node, finding int64
		if err := s.m.pg.QueryRow(`INSERT INTO exploration_nodes(exploration_id,kind,state,payload) VALUES($1,'finding','confirmed','{}') RETURNING id`, task.Store.ID()).Scan(&node); err != nil {
			t.Fatal(err)
		}
		if err := s.m.pg.QueryRow(`INSERT INTO findings(task_id,node_id,name,report) VALUES($1,$2,'fixture','<report> & exact') RETURNING id`, id, node).Scan(&finding); err != nil {
			t.Fatal(err)
		}
		findingIDs = append(findingIDs, finding)
	}
	var caseID int64
	if err := s.m.pg.QueryRow(`INSERT INTO finding_cases(task_id,origin_task_id,title,reason) VALUES($1,$1,'case','fixture') RETURNING id`, id).Scan(&caseID); err != nil {
		t.Fatal(err)
	}
	for _, finding := range findingIDs {
		if _, err := s.m.pg.Exec(`INSERT INTO finding_case_members(case_id,finding_id) VALUES($1,$2)`, caseID, finding); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.m.pg.Exec(`INSERT INTO finding_case_events(case_id,action,actor,reason,finding_ids) VALUES($1,'fixture','human','all data','[]')`, caseID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO task_intercept_rules(task_id,kind,pattern) VALUES($1,'exact_domain','fixture.invalid')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO main_sessions(exploration_id,seq) VALUES($1,3) ON CONFLICT DO NOTHING`, task.Store.ID()); err != nil {
		t.Fatal(err)
	}
	var approvalID, conversationID int64
	if err := s.m.pg.QueryRow(`INSERT INTO conversations(title,agent_key) VALUES('task retest fixture','retester') RETURNING id`).Scan(&conversationID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, conversationID) })
	if err := s.m.pg.QueryRow(`INSERT INTO intercept_pending(task_id,tool_name,status,decision_source) VALUES($1,'fixture','allowed','manual') RETURNING id`, task.ID).Scan(&approvalID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO side_question_sessions(session_key,approval_id,run_id,version,snapshot) VALUES($1,$2,1,1,'{}')`, "approval-fixture-"+task.ID, approvalID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO finding_retests(finding_id,conversation_id,status,snapshot) VALUES($1,$2,'completed','{}')`, findingIDs[0], conversationID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO llm_records(task_id,session_id,raw_request,raw_response) VALUES($1,'fixture','<request> & original','<response> & original')`, task.ID); err != nil {
		t.Fatal(err)
	}
	conversationKey := "conv-" + strconv.FormatInt(conversationID, 10)
	if _, err := s.m.pg.Exec(`INSERT INTO llm_records(session_id,raw_response) VALUES($1,'related conversation raw response')`, conversationKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM llm_records WHERE session_id=$1`, conversationKey) })
	for _, relative := range []string{filepath.Join("sessions", conversationKey, "attachment.bin"), filepath.Join("transcripts", conversationKey+".jsonl"), filepath.Join("noa", "chat-"+conversationKey, "original.json")} {
		file := filepath.Join(s.m.dir, relative)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte{0, 255, 10}, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s.skillDir = t.TempDir()
	if err := os.Mkdir(filepath.Join(s.skillDir, "archive-fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.skillDir, "archive-fixture", "SKILL.md"), []byte("original skill instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO skill_usage(skill,task_id,found) VALUES('archive-fixture',$1,true)`, id); err != nil {
		t.Fatal(err)
	}
	job, err := s.prepareTaskArchive(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if job.StorageKind != directoryArchiveKind || job.RequiredBytes <= job.EstimatedBytes || job.SnapshotAt == nil {
		t.Fatalf("missing admission metadata: %+v", job)
	}
	if err := verifyDirectoryArchive(job.PreparedPath, job.SHA256); err != nil {
		t.Fatal(err)
	}
	archivedSkill, err := os.ReadFile(filepath.Join(job.PreparedPath, "dependencies", "skills", "archive-fixture", "SKILL.md"))
	if err != nil || string(archivedSkill) != "original skill instructions" {
		t.Fatalf("skill omitted: %q %v", archivedSkill, err)
	}
	if err := s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	job, err = s.m.pg.GetTaskArchive(job.ID)
	if err != nil || job.State != db.ArchiveReady {
		t.Fatalf("archive=%+v err=%v", job, err)
	}
	if _, err := os.Stat(filepath.Join(s.m.dir, "tasks", task.ID)); !os.IsNotExist(err) {
		t.Fatal("owned workspace not moved out of hot store")
	}
	var count int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM finding_cases WHERE task_id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("case compaction count=%d err=%v", count, err)
	}
	if _, err := s.m.pg.QueueTaskArchiveRestore(job.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(filepath.Join(s.m.dir, "tasks", task.ID, "workspace", "binary.bin"))
	if err != nil || string(file) != string([]byte{0, 1, 2, 255, 0, 10}) {
		t.Fatalf("file mismatch %v", err)
	}
	if err := verifyDirectoryArchive(job.ArchivePath, job.SHA256); err != nil {
		t.Fatalf("restore consumed or modified retained archive: %v", err)
	}
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM finding_case_members WHERE case_id=$1`, caseID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("members count=%d err=%v", count, err)
	}
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM task_intercept_rules WHERE task_id=$1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rules count=%d err=%v", count, err)
	}
	if _, err := os.Stat(filepath.Join(s.m.dir, "noa", "exp"+strconv.FormatInt(task.Store.ID(), 10)+"-planner", "summary.json")); err != nil {
		t.Fatal(err)
	}
	current, err := s.m.pg.GetTask(id)
	if err != nil || current == nil || !current.Paused {
		t.Fatal("restored task did not remain paused")
	}
	for _, query := range []string{`SELECT count(*) FROM side_question_sessions WHERE approval_id=$1`, `SELECT count(*) FROM intercept_pending WHERE id=$1`} {
		if err = s.m.pg.QueryRow(query, approvalID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("approval history lost: %d %v", count, err)
		}
	}
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM finding_retests WHERE conversation_id=$1`, conversationID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retest lost: %d %v", count, err)
	}
	var rawResponse string
	if err = s.m.pg.QueryRow(`SELECT raw_response FROM llm_records WHERE task_id=$1`, task.ID).Scan(&rawResponse); err != nil || rawResponse != "<response> & original" {
		t.Fatalf("raw LLM record changed: %q %v", rawResponse, err)
	}
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM llm_records WHERE session_id=$1`, conversationKey).Scan(&count); err != nil || count != 1 {
		t.Fatalf("shared conversation LLM duplicate/lost: %d %v", count, err)
	}
}

func TestDirectoryArchiveRejectsLateDatabaseChangesAndRollsBack(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	job, err := s.prepareTaskArchive(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`UPDATE tasks SET remark='late write' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := s.runOneTaskArchiveJob(); err == nil || !strings.Contains(err.Error(), "发生变化") {
		t.Fatalf("late write accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.m.dir, "tasks", task.ID, "workspace", "binary.bin")); err != nil {
		t.Fatalf("original not rolled back: %v", err)
	}
	if err := verifyDirectoryArchive(job.PreparedPath, job.SHA256); err != nil {
		t.Fatalf("accepted snapshot lost: %v", err)
	}
	var raw map[string]json.RawMessage
	f, err := os.ReadFile(filepath.Join(job.PreparedPath, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(f, &raw); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryArchivePreparingCannotBeClaimedAndRecovers(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	job, err := s.m.pg.QueueTaskArchive(id, &db.ArchivePreparation{})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.m.pg.ClaimTaskArchiveJob(t.Context())
	if err != nil || claimed != nil {
		t.Fatalf("incomplete snapshot claimed: %+v %v", claimed, err)
	}
	if err = s.m.pg.RecoverTaskArchiveJobs(); err != nil {
		t.Fatal(err)
	}
	job, err = s.m.pg.GetTaskArchive(job.ID)
	if err != nil || job.State != db.ArchiveFailed {
		t.Fatalf("interrupted admission=%+v err=%v", job, err)
	}
	if _, err = os.Stat(filepath.Join(s.m.dir, "tasks", task.ID, "workspace", "binary.bin")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.prepareTaskArchive(t.Context(), id); err != nil {
		t.Fatalf("explicit retry failed: %v", err)
	}
}

func TestDirectoryArchiveRejectsLateFileChangesWithoutDeletingOriginal(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	job, err := s.prepareTaskArchive(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(s.m.dir, "tasks", task.ID, "workspace", "binary.bin")
	if err = os.WriteFile(file, []byte("late bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err == nil {
		t.Fatal("late file write accepted")
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "late bytes" {
		t.Fatalf("late original lost: %q %v", data, err)
	}
	if err = verifyDirectoryArchive(job.PreparedPath, job.SHA256); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryArchiveRechecksSpaceAfterPreview(t *testing.T) {
	s, _, id := directoryArchiveFixture(t)
	s.archiveSpaceQuery = func(string) (uint64, uint64, error) { return 1 << 40, 4096, nil }
	plan, _, err := s.estimateArchive(t.Context(), id)
	if err != nil || !plan.Space.Sufficient {
		t.Fatalf("preview=%+v %v", plan, err)
	}
	s.archiveSpaceQuery = func(string) (uint64, uint64, error) { return 1, 4096, nil }
	if _, err = s.prepareTaskArchive(t.Context(), id); err == nil {
		t.Fatal("stale preview bypassed capacity check")
	}
	job, err := s.m.pg.GetTaskArchiveByTask(id)
	if err != nil || job != nil {
		t.Fatalf("insufficient capacity queued a job: %+v %v", job, err)
	}
}

func TestDirectoryArchiveCapacityChangedWhileObtainingBarrier(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	checks := 0
	s.archiveSpaceQuery = func(string) (uint64, uint64, error) {
		checks++
		if checks == 1 {
			return 1 << 40, 4096, nil
		}
		return 0, 4096, nil
	}
	if _, err := s.prepareTaskArchive(t.Context(), id); err == nil {
		t.Fatal("capacity not rechecked under barrier")
	}
	if checks != 2 {
		t.Fatalf("disk queries=%d", checks)
	}
	if row, err := s.m.pg.GetTaskArchiveByTask(id); err != nil || row != nil {
		t.Fatalf("queued despite insufficient final space: %+v %v", row, err)
	}
	if s.engine.IsDeleting(task.ID) {
		t.Fatal("barrier not released")
	}
}

func TestDirectoryArchiveEstimateEndpointNormalizesAndDoesNotQueue(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	s.archiveSpaceQuery = func(string) (uint64, uint64, error) { return 1 << 40, 4096, nil }
	r := httptest.NewRequest("POST", "/api/tasks/archive/estimate", strings.NewReader(`{"task_ids":["`+task.ID+`","`+task.ID+`"]}`))
	w := httptest.NewRecorder()
	s.estimateTaskArchives(w, r)
	var estimate archiveSpaceEstimate
	if err := json.Unmarshal(w.Body.Bytes(), &estimate); err != nil || w.Code != 200 || !estimate.Sufficient || estimate.EstimatedBytes == 0 {
		t.Fatalf("estimate status=%d body=%s err=%v", w.Code, w.Body.String(), err)
	}
	plan, _, err := s.estimateArchive(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	// JSON time fractions can differ by a few bytes between two snapshots.
	delta := int64(estimate.EstimatedBytes) - int64(plan.Space.EstimatedBytes)
	if delta < -64 || delta > 64 || estimate.FileCount != plan.Space.FileCount {
		t.Fatalf("duplicate task charged twice: preview=%+v single=%+v", estimate, plan.Space)
	}
	if job, err := s.m.pg.GetTaskArchiveByTask(id); err != nil || job != nil {
		t.Fatalf("preview queued task: %+v %v", job, err)
	}
}

func TestDirectoryArchiveInterruptedOriginalStagingRollsBack(t *testing.T) {
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
	if err = s.m.pg.RecoverTaskArchiveJobs(); err != nil {
		t.Fatal(err)
	}
	if err = recoverTaskArchiveStages(s.m.dir, s.m.pg); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(s.m.dir, "tasks", task.ID, "workspace", "binary.bin")); err != nil {
		t.Fatalf("original not recovered: %v", err)
	}
	if err = verifyDirectoryArchive(job.PreparedPath, job.SHA256); err != nil {
		t.Fatalf("verified archive lost during rollback: %v", err)
	}
}

func TestDirectoryArchiveUnknownTaskTableFailsClosed(t *testing.T) {
	s, _, id := directoryArchiveFixture(t)
	if _, err := s.m.pg.Exec(`CREATE TABLE archive_future_fixture(task_id bigint)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DROP TABLE archive_future_fixture`) })
	if _, err := s.prepareTaskArchive(t.Context(), id); err == nil || !strings.Contains(err.Error(), "archive_future_fixture") {
		t.Fatalf("uncovered task data silently skipped: %v", err)
	}
}

func TestDirectoryArchiveDeleteRemovesOnlySelectedDirectory(t *testing.T) {
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
	unrelated := filepath.Join(taskArchiveRoot(s.m.dir), "retained-unrelated")
	if err = os.Mkdir(unrelated, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = s.m.pg.QueueTaskArchiveDelete(job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.runOneTaskArchiveJob(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(job.ArchivePath); !os.IsNotExist(err) {
		t.Fatalf("selected archive still present: %v", err)
	}
	if _, err = os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated archive removed: %v", err)
	}
	if row, err := s.m.pg.GetTaskArchive(job.ID); err != nil || row != nil {
		t.Fatalf("selected metadata remained: %+v %v", row, err)
	}
}

func TestDirectoryArchiveAmbiguousCaseOwnershipCannotDeleteOtherTaskMembers(t *testing.T) {
	s, task, id := directoryArchiveFixture(t)
	other, err := s.m.CreateTask("other case owner", "independent task", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	otherID, _ := strconv.ParseInt(other.ID, 10, 64)
	t.Cleanup(func() { _ = s.m.pg.DeleteTask(otherID) })
	var node, finding, caseID int64
	if err = s.m.pg.QueryRow(`INSERT INTO exploration_nodes(exploration_id,kind,state,payload) VALUES($1,'finding','confirmed','{}') RETURNING id`, task.Store.ID()).Scan(&node); err != nil {
		t.Fatal(err)
	}
	if err = s.m.pg.QueryRow(`INSERT INTO findings(task_id,node_id,name) VALUES($1,$2,'ambiguous fixture') RETURNING id`, id, node).Scan(&finding); err != nil {
		t.Fatal(err)
	}
	if err = s.m.pg.QueryRow(`INSERT INTO finding_cases(task_id,origin_task_id,title,reason) VALUES($1,$1,'other owner','fixture') RETURNING id`, otherID).Scan(&caseID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.m.pg.Exec(`INSERT INTO finding_case_members(case_id,finding_id) VALUES($1,$2)`, caseID, finding); err != nil {
		t.Fatal(err)
	}
	if _, err = s.prepareTaskArchive(t.Context(), id); err == nil || !strings.Contains(err.Error(), "归属不一致") {
		t.Fatalf("ambiguous ownership accepted: %v", err)
	}
	var count int
	if err = s.m.pg.QueryRow(`SELECT count(*) FROM finding_case_members WHERE case_id=$1`, caseID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("other task case changed: %d %v", count, err)
	}
}
