package db

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Task-owned children without task_id follow their actual foreign keys.
type taskArchiveQuery struct {
	name, query string
	args        []any
}

func taskArchiveDependencies(tx *sql.Tx, taskID, expID int64) (map[string]json.RawMessage, error) {
	conversations := strings.ReplaceAll(archiveConversationIDsQuery(), "$2", "$1::text")
	agents := `SELECT id FROM agents WHERE role IN ('goals','main','planner','worker') OR key IN ('reporter','retester') OR key IN (SELECT agent_key FROM tool_usage WHERE task_id=$1 OR exploration_id=$2 UNION SELECT agent_key FROM skill_usage WHERE task_id=$1 OR exploration_id=$2) OR key IN (SELECT agent_key FROM conversations WHERE id IN (` + conversations + `))`
	queries := []taskArchiveQuery{
		{"agents", `SELECT * FROM agents WHERE id IN (` + agents + `) ORDER BY id`, []any{taskID, expID}},
		{"agent_prompts", `SELECT * FROM agent_prompts WHERE agent_id IN (` + agents + `) ORDER BY id`, []any{taskID, expID}},
		{"agent_visibility", `SELECT * FROM agent_visibility WHERE agent_id IN (` + agents + `) ORDER BY agent_id,resource_id,mcp_tool_name`, []any{taskID, expID}},
		{"agent_skill_visibility", `SELECT * FROM agent_skill_visibility WHERE agent_id IN (` + agents + `) ORDER BY agent_id,skill_name`, []any{taskID, expID}},
		{"tools", `SELECT * FROM tools WHERE key IN (SELECT tool_key FROM tool_usage WHERE task_id=$1 OR exploration_id=$2) ORDER BY key`, []any{taskID, expID}},
		{"mcp_servers", `SELECT id,name,transport,enabled,insecure,created_at,updated_at FROM mcp_servers WHERE id IN (SELECT resource_id FROM agent_visibility WHERE agent_id IN (` + agents + `)) ORDER BY id`, []any{taskID, expID}},
		{"llm_profiles", `SELECT id,name,format,base_url,model,rate_per_second,rate_per_minute,max_concurrency,context_window_k,streaming,max_tokens,max_tokens_field,session_header_key FROM llm_profiles WHERE id IN (SELECT llm_profile_id FROM tasks WHERE id=$1 UNION SELECT active_llm_profile_id FROM tasks WHERE id=$1 UNION SELECT profile_id FROM task_llm_profiles WHERE task_id=$1 UNION SELECT llm_profile_id FROM conversations WHERE id IN (` + conversations + `) UNION SELECT llm_profile_id FROM agents WHERE id IN (` + agents + `)) ORDER BY id`, []any{taskID, expID}},
		{"task_categories", `SELECT c.* FROM task_categories c JOIN tasks t ON t.category_id=c.id WHERE t.id=$1`, []any{taskID}},
		{"companies", `SELECT c.* FROM companies c JOIN tasks t ON t.company_id=c.id WHERE t.id=$1`, []any{taskID}},
	}
	out := map[string]json.RawMessage{}
	for _, q := range queries {
		raw, _, err := queryArchiveRows(tx, q.query, q.args...)
		if err != nil {
			return nil, fmt.Errorf("归档依赖 %s: %w", q.name, err)
		}
		out[q.name] = raw
	}
	return out, nil
}

func archiveConversationIDsQuery() string {
	return `SELECT conversation_id FROM finding_retests WHERE finding_id IN (SELECT id FROM findings WHERE task_id=$1) UNION SELECT conversation_id FROM finding_case_review_runs WHERE task_id=$1 UNION SELECT conversation_id FROM side_question_sessions WHERE task_id=$1 OR exploration_id=(SELECT exploration_id FROM tasks WHERE id=$1) OR approval_id IN (SELECT id FROM intercept_pending WHERE task_id=$2) UNION SELECT conversation_id FROM intercept_pending WHERE task_id=$2`
}

func archiveSideSessionKeysQuery() string {
	conversations := archiveConversationIDsQuery()
	return `SELECT session_key FROM side_question_sessions WHERE task_id=$1 OR exploration_id=$3
OR conversation_id IN (` + conversations + `)
OR approval_id IN (SELECT id FROM intercept_pending WHERE task_id=$2 OR conversation_id IN (` + conversations + `))`
}

func additionalTaskArchiveQueries(taskID, expID int64) []taskArchiveQuery {
	findings := `SELECT id FROM findings WHERE task_id=$1`
	cases := `SELECT id FROM finding_cases WHERE task_id=$1`
	conversations := archiveConversationIDsQuery()
	return []taskArchiveQuery{
		{"main_sessions", `SELECT * FROM main_sessions WHERE exploration_id=$1 ORDER BY seq`, []any{expID}},
		{"task_intercept_rules", `SELECT * FROM task_intercept_rules WHERE task_id=$1 ORDER BY id`, []any{taskID}},
		{"finding_cases", `SELECT * FROM finding_cases WHERE task_id=$1 ORDER BY id`, []any{taskID}},
		{"finding_case_members", `SELECT * FROM finding_case_members WHERE finding_id IN (` + findings + `) ORDER BY finding_id`, []any{taskID}},
		{"finding_case_events", `SELECT * FROM finding_case_events WHERE case_id IN (` + cases + `) ORDER BY id`, []any{taskID}},
		{"finding_case_suggestions", `SELECT * FROM finding_case_suggestions WHERE task_id=$1 ORDER BY id`, []any{taskID}},
		{"finding_case_suggestion_members", `SELECT m.* FROM finding_case_suggestion_members m JOIN finding_case_suggestions s ON s.id=m.suggestion_id WHERE s.task_id=$1 ORDER BY suggestion_id,finding_id`, []any{taskID}},
		{"finding_case_blocks", `SELECT * FROM finding_case_blocks WHERE left_id IN (` + findings + `) OR right_id IN (` + findings + `) ORDER BY left_id,right_id`, []any{taskID}},
		{"finding_retests", `SELECT * FROM finding_retests WHERE finding_id IN (` + findings + `) ORDER BY id`, []any{taskID}},
		{"finding_case_review_runs", `SELECT * FROM finding_case_review_runs WHERE task_id=$1 ORDER BY conversation_id`, []any{taskID}},
		{"conversations", `SELECT * FROM conversations WHERE id IN (` + conversations + `) ORDER BY id`, []any{taskID, strconv.FormatInt(taskID, 10)}},
		{"conversation_activities", `SELECT * FROM conversation_activities WHERE conversation_id IN (` + conversations + `) ORDER BY id`, []any{taskID, strconv.FormatInt(taskID, 10)}},
		{"task_schedule_tasks", `SELECT * FROM task_schedule_tasks WHERE task_id=$1 ORDER BY schedule_id`, []any{taskID}},
		{"task_schedule_history", `SELECT * FROM task_schedule_history WHERE task_id=$1 ORDER BY id`, []any{taskID}},
		{"notification_events", `SELECT * FROM notification_events WHERE finding_id IN (` + findings + `) ORDER BY id`, []any{taskID}},
		{"notification_deliveries", `SELECT d.* FROM notification_deliveries d JOIN notification_events e ON e.id=d.event_id WHERE e.finding_id IN (` + findings + `) ORDER BY d.id`, []any{taskID}},
	}
}

func validateArchiveCoverage(tx *sql.Tx, queries []taskArchiveQuery) error {
	covered := map[string]bool{"task_archives": true}
	for _, q := range queries {
		covered[q.name] = true
	}
	rows, err := tx.Query(`SELECT DISTINCT table_name FROM information_schema.columns WHERE table_schema=current_schema() AND column_name IN ('task_id','exploration_id','origin_task_id') ORDER BY table_name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if !covered[name] {
			return fmt.Errorf("任务数据表 %s 尚未纳入完整归档，已阻止归档，原数据保留", name)
		}
	}
	return rows.Err()
}

func validateTaskArchiveOwnership(tx *sql.Tx, taskID, expID int64) error {
	var ambiguous bool
	err := tx.QueryRow(`SELECT EXISTS(
SELECT 1 FROM finding_case_members m JOIN findings f ON f.id=m.finding_id JOIN finding_cases c ON c.id=m.case_id
WHERE (f.task_id=$1 AND c.task_id IS DISTINCT FROM $1) OR (c.task_id=$1 AND f.task_id IS DISTINCT FROM $1)
) OR EXISTS(
SELECT 1 FROM side_question_sessions s JOIN intercept_pending p ON p.id=s.approval_id
WHERE p.task_id=$1::text AND ((s.task_id IS NOT NULL AND s.task_id<>$1) OR (s.exploration_id IS NOT NULL AND s.exploration_id<>$2))
) OR EXISTS(SELECT 1 FROM side_question_sessions s WHERE
(s.task_id=$1 AND s.exploration_id IS NOT NULL AND s.exploration_id<>$2)
OR (s.exploration_id=$2 AND s.task_id IS NOT NULL AND s.task_id<>$1))
OR EXISTS(SELECT 1 FROM llm_records WHERE session_id LIKE 'exp'||$2::bigint::text||'-%'
AND COALESCE(task_id,'') NOT IN ('',$1::text))`, taskID, expID).Scan(&ambiguous)
	if err != nil {
		return err
	}
	if ambiguous {
		return fmt.Errorf("任务归属不一致：漏洞归并或审批侧会话跨越其他任务，已阻止归档，原数据保留")
	}
	return nil
}

func validateArchiveUsageOwnership(tables map[string]json.RawMessage, taskID, expID int64) error {
	for _, name := range []string{"llm_usage", "skill_usage", "tool_usage"} {
		d := json.NewDecoder(bytes.NewReader(tables[name]))
		if _, err := d.Token(); err != nil {
			return err
		}
		for d.More() {
			var scope struct {
				TaskID    json.RawMessage `json:"task_id"`
				ExpID     int64           `json:"exploration_id"`
				SessionID string          `json:"session_id"`
			}
			if err := d.Decode(&scope); err != nil {
				return err
			}
			raw := strings.Trim(string(scope.TaskID), "\"")
			owner, _ := strconv.ParseInt(raw, 10, 64)
			matchesExp := scope.ExpID == expID || strings.HasPrefix(scope.SessionID, fmt.Sprintf("exp%d-", expID))
			if (owner > 0 && owner != taskID && matchesExp) || (owner == taskID && scope.ExpID > 0 && scope.ExpID != expID) {
				return fmt.Errorf("任务归属不一致：%s 的任务与探索/会话字段冲突，原数据保留", name)
			}
		}
	}
	return nil
}

// Lock the exported relations in one deterministic order and re-read the task
// scope inside the compaction transaction. A late write is a conflict, not data
// that may silently be deleted after a stale snapshot was accepted.
func validateArchiveBeforeCompaction(tx *sql.Tx, snapshot *TaskArchiveSnapshot) error {
	if snapshot.SnapshotAt.IsZero() {
		return nil
	}
	queries := allTaskArchiveQueries(snapshot.TaskID, snapshot.ExplorationID)
	var names []string
	for _, q := range queries {
		names = append(names, q.name)
	}
	sort.Strings(names)
	if _, err := tx.Exec(`LOCK TABLE ` + strings.Join(names, ",") + ` IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	if err := validateTaskArchiveOwnership(tx, snapshot.TaskID, snapshot.ExplorationID); err != nil {
		return err
	}
	for _, q := range queries {
		if q.name == "llm_records" {
			h := sha256.New()
			count, err := streamArchiveRows(tx, h, q.query, q.args...)
			if err != nil {
				return err
			}
			if count != snapshot.DataCounts[q.name] || hex.EncodeToString(h.Sum(nil)) != snapshot.LLMDigest {
				return fmt.Errorf("归档受理后 %s 发生变化，未清理原数据", q.name)
			}
			continue
		}
		raw, count, err := queryArchiveRows(tx, q.query, q.args...)
		if err != nil {
			return err
		}
		before, err := canonicalArchiveJSON(snapshot.Tables[q.name])
		if err != nil {
			return err
		}
		after, err := canonicalArchiveJSON(raw)
		if err != nil {
			return err
		}
		if count != snapshot.DataCounts[q.name] || !bytes.Equal(before, after) {
			return fmt.Errorf("归档受理后 %s 发生变化，未清理原数据", q.name)
		}
	}
	return nil
}

func canonicalArchiveJSON(raw []byte) ([]byte, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func compactAdditionalArchiveRows(tx *sql.Tx, taskID, expID int64) error {
	// Delete memberships before cases; member-delete triggers may append an event,
	// so remove events only after those triggers have run. The snapshot is already
	// durable before this transaction starts.
	for _, query := range []string{
		`DELETE FROM finding_case_members WHERE finding_id IN (SELECT id FROM findings WHERE task_id=$1)`,
		`DELETE FROM finding_case_events WHERE case_id IN (SELECT id FROM finding_cases WHERE task_id=$1)`,
		`DELETE FROM finding_case_suggestions WHERE task_id=$1`,
		`DELETE FROM finding_cases WHERE task_id=$1`,
		`DELETE FROM finding_case_review_runs WHERE task_id=$1`,
		`DELETE FROM task_intercept_rules WHERE task_id=$1`,
	} {
		if _, err := tx.Exec(query, taskID); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`DELETE FROM main_sessions WHERE exploration_id=$1`, expID)
	return err
}

// Conversations and notification history are shared/global records retained in
// the hot database. Archive their exact task-related snapshot without deleting
// another surface's records; restoration never overwrites existing global rows.
func restoreArchiveReferenceRows(tx *sql.Tx, table string, raw json.RawMessage, owners ...*archiveRestoreOwnership) error {
	allowed := map[string]bool{"conversations": true, "conversation_activities": true, "task_schedule_tasks": true, "task_schedule_history": true, "notification_events": true, "notification_deliveries": true, "llm_records": true, "llm_usage": true, "skill_usage": true, "tool_usage": true, "side_question_sessions": true, "side_question_requests": true, "intercept_pending": true}
	if !allowed[table] {
		return fmt.Errorf("不支持的归档依赖表 %s", table)
	}
	if rawRowCount(raw) == 0 {
		return nil
	}
	if len(owners) > 0 && owners[0] != nil {
		rows, err := decodeArchiveRows(raw)
		if err != nil {
			return err
		}
		owned, shared := []map[string]any{}, []map[string]any{}
		for _, row := range rows {
			if owners[0].owns(table, row) {
				owned = append(owned, row)
			} else {
				shared = append(shared, row)
			}
		}
		if len(owned) > 0 {
			encoded, err := json.Marshal(owned)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(`INSERT INTO `+table+` SELECT * FROM json_populate_recordset(NULL::`+table+`,$1::json)`, string(encoded)); err != nil {
				return fmt.Errorf("恢复任务独占记录 %s 冲突: %w", table, err)
			}
		}
		if len(shared) == 0 {
			return nil
		}
		raw, err = json.Marshal(shared)
		if err != nil {
			return err
		}
	}
	_, err := tx.Exec(`INSERT INTO `+table+` SELECT * FROM json_populate_recordset(NULL::`+table+`,$1::json) ON CONFLICT DO NOTHING`, string(raw))
	return err
}

type archiveRestoreOwnership struct {
	taskID, expID int64
	approvals     map[int64]bool
	sessions      map[string]bool
}

func archiveTaskID(value any) int64 {
	if text, ok := value.(string); ok {
		id, _ := strconv.ParseInt(text, 10, 64)
		return id
	}
	id, _ := jsonInt64(value)
	return id
}

func (o *archiveRestoreOwnership) owns(table string, row map[string]any) bool {
	if archiveTaskID(row["task_id"]) == o.taskID {
		return true
	}
	if expID, ok := jsonInt64(row["exploration_id"]); ok && expID == o.expID {
		return true
	}
	if table == "llm_records" || table == "skill_usage" || table == "tool_usage" {
		session, _ := row["session_id"].(string)
		if strings.HasPrefix(session, fmt.Sprintf("exp%d-", o.expID)) {
			return true
		}
	}
	if table == "side_question_sessions" {
		id, _ := jsonInt64(row["approval_id"])
		return o.approvals[id]
	}
	if table == "side_question_requests" {
		key, _ := row["session_key"].(string)
		return o.sessions[key]
	}
	return false
}

func newArchiveRestoreOwnership(snapshot *TaskArchiveSnapshot) (*archiveRestoreOwnership, error) {
	o := &archiveRestoreOwnership{taskID: snapshot.TaskID, expID: snapshot.ExplorationID, approvals: map[int64]bool{}, sessions: map[string]bool{}}
	for _, table := range []string{"intercept_pending", "side_question_sessions"} {
		rows, err := decodeArchiveRows(snapshot.Tables[table])
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if !o.owns(table, row) {
				continue
			}
			if table == "intercept_pending" {
				id, _ := jsonInt64(row["id"])
				o.approvals[id] = true
			} else {
				key, _ := row["session_key"].(string)
				o.sessions[key] = true
			}
		}
	}
	return o, nil
}

func restoreArchiveConversations(tx *sql.Tx, tables map[string]json.RawMessage) error {
	for _, name := range []string{"conversations", "conversation_activities"} {
		if err := restoreArchiveReferenceRows(tx, name, tables[name]); err != nil {
			return err
		}
	}
	return nil
}

func restoreAdditionalArchiveRows(tx *sql.Tx, tables map[string]json.RawMessage) error {
	for _, name := range []string{"main_sessions", "task_intercept_rules", "finding_cases", "finding_case_members", "finding_case_events", "finding_case_suggestions", "finding_case_suggestion_members", "finding_case_blocks", "finding_retests", "finding_case_review_runs"} {
		if rawRowCount(tables[name]) == 0 {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO `+name+` SELECT * FROM json_populate_recordset(NULL::`+name+`,$1::json)`, string(tables[name])); err != nil {
			return fmt.Errorf("恢复 %s: %w", name, err)
		}
	}
	for _, name := range []string{"task_schedule_tasks", "task_schedule_history", "notification_events", "notification_deliveries"} {
		if err := restoreArchiveReferenceRows(tx, name, tables[name]); err != nil {
			return err
		}
	}
	return nil
}

func allTaskArchiveQueries(taskID, expID int64) []taskArchiveQuery {
	conversationSessions := `SELECT 'conv-' || id::text FROM conversations WHERE id IN (` + archiveConversationIDsQuery() + `)
UNION SELECT 'exp' || COALESCE(s.exploration_id,0)::text || '-btw-' || r.id
FROM side_question_requests r JOIN side_question_sessions s ON s.session_key=r.session_key
WHERE s.session_key IN (` + archiveSideSessionKeysQuery() + `)`
	queries := []taskArchiveQuery{
		{"tasks", `SELECT * FROM tasks WHERE id=$1`, []any{taskID}},
		{"explorations", `SELECT * FROM explorations WHERE id=$1`, []any{expID}},
		{"exploration_nodes", `SELECT * FROM exploration_nodes WHERE exploration_id=$1 ORDER BY id`, []any{expID}},
		{"exploration_edges", `SELECT * FROM exploration_edges WHERE exploration_id=$1 ORDER BY src_id,dst_id`, []any{expID}},
		{"exploration_anchors", `SELECT anchor.* FROM exploration_anchors anchor JOIN exploration_nodes node ON node.id=anchor.node_id WHERE node.exploration_id=$1 ORDER BY node_id,asset_id`, []any{expID}},
		{"task_constraints", `SELECT * FROM task_constraints WHERE exploration_id=$1 ORDER BY id`, []any{expID}},
		{"activity", `SELECT * FROM activity WHERE exploration_id=$1 ORDER BY id`, []any{expID}},
		{"task_relations", `SELECT * FROM task_relations WHERE task_id=$1 ORDER BY created_at,source_task_id`, []any{taskID}},
		{"task_asset_links", `SELECT * FROM task_asset_links WHERE task_id=$1 ORDER BY asset_id`, []any{taskID}},
		{"task_llm_profiles", `SELECT * FROM task_llm_profiles WHERE task_id=$1 ORDER BY position`, []any{taskID}},
		{"task_scope", `SELECT * FROM task_scope WHERE task_id=$1 ORDER BY id`, []any{taskID}},
		{"findings", `SELECT * FROM findings WHERE task_id=$1 ORDER BY id`, []any{taskID}},
		{"task_intelligence", `SELECT * FROM task_intelligence WHERE task_id=$1 ORDER BY id`, []any{taskID}},
		{"finding_traffic_bindings", `SELECT b.* FROM finding_traffic_bindings b JOIN findings f ON f.id=b.finding_id WHERE f.task_id=$1 ORDER BY b.finding_id,b.position,b.id`, []any{taskID}},
		{"traffic_evidence_snapshots", `SELECT s.* FROM traffic_evidence_snapshots s WHERE EXISTS(SELECT 1 FROM finding_traffic_bindings b JOIN findings f ON f.id=b.finding_id WHERE b.snapshot_id=s.id AND f.task_id=$1) ORDER BY s.id`, []any{taskID}},
		{"llm_records", `SELECT * FROM llm_records WHERE COALESCE(task_id,'')=$2 OR session_id LIKE 'exp'||$3::bigint::text||'-%' OR session_id IN (` + conversationSessions + `) ORDER BY id`, []any{taskID, strconv.FormatInt(taskID, 10), expID}},
		{"llm_usage", `SELECT * FROM llm_usage WHERE COALESCE(task_id,'')=$1 OR exploration_id=$2 ORDER BY id`, []any{strconv.FormatInt(taskID, 10), expID}},
		{"skill_usage", `SELECT * FROM skill_usage WHERE task_id=$1 OR exploration_id=$3 OR session_id LIKE 'exp'||$3::bigint::text||'-%' OR session_id IN (` + conversationSessions + `) ORDER BY id`, []any{taskID, strconv.FormatInt(taskID, 10), expID}},
		{"tool_usage", `SELECT * FROM tool_usage WHERE task_id=$1 OR exploration_id=$3 OR session_id LIKE 'exp'||$3::bigint::text||'-%' OR session_id IN (` + conversationSessions + `) ORDER BY id`, []any{taskID, strconv.FormatInt(taskID, 10), expID}},
		{"intercept_pending", `SELECT * FROM intercept_pending WHERE COALESCE(task_id,'')=$2 OR conversation_id IN (` + archiveConversationIDsQuery() + `) ORDER BY id`, []any{taskID, strconv.FormatInt(taskID, 10)}},
		{"side_question_sessions", `SELECT * FROM side_question_sessions WHERE session_key IN (` + archiveSideSessionKeysQuery() + `) ORDER BY session_key`, []any{taskID, strconv.FormatInt(taskID, 10), expID}},
		{"side_question_requests", `SELECT * FROM side_question_requests WHERE session_key IN (` + archiveSideSessionKeysQuery() + `) ORDER BY ordinal`, []any{taskID, strconv.FormatInt(taskID, 10), expID}},
		{"assets", `SELECT asset.* FROM assets asset WHERE asset.id IN (` + archiveAssetIDsQuery() + `) ORDER BY asset.id`, []any{taskID, expID}},
	}
	queries = append(queries, additionalTaskArchiveQueries(taskID, expID)...)
	return queries
}
