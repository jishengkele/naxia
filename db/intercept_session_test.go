package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestInterceptSessionProjectionAndLegacy(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	scope := "approval-session-" + t.Name()
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM intercept_pending WHERE task_id=$1`, scope) })
	expected := map[int64]string{}
	for _, session := range []string{"plan", "main:0", "main:3", ""} {
		var audit *InterceptAudit
		if session != "" {
			audit = &InterceptAudit{Session: session, UserMessage: "private-session-snapshot", InitialAction: "ask"}
		}
		id, err := d.CreateInterceptPending(0, 0, scope, "fixture", "Write", []byte(`{}`), "approve", audit)
		if err != nil {
			t.Fatal(err)
		}
		expected[id] = session
		pending, err := d.GetInterceptPending(id)
		if err != nil || pending == nil || pending.Session != session {
			t.Fatalf("single pending=%+v err=%v", pending, err)
		}
		detail, err := d.GetInterceptDetail(id)
		if err != nil || detail == nil || detail.Session != session || (detail.Audit == nil) != (audit == nil) {
			t.Fatalf("detail=%+v err=%v", detail, err)
		}
	}
	check := func(t *testing.T, rows []InterceptPending, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, row := range rows {
			want, ok := expected[row.ID]
			if !ok {
				continue
			}
			seen++
			if row.Session != want {
				t.Fatalf("session=%q want=%q", row.Session, want)
			}
			raw, err := json.Marshal(row)
			if err != nil || strings.Contains(string(raw), "private-session-snapshot") || strings.Contains(string(raw), `"audit"`) {
				t.Fatalf("list exposed full audit: %s %v", raw, err)
			}
			if want == "" && strings.Contains(string(raw), `"session"`) {
				t.Fatalf("legacy session should remain optional: %s", raw)
			}
		}
		if seen != len(expected) {
			t.Fatalf("session rows=%d want=%d", seen, len(expected))
		}
	}
	rows, err := d.ListPendingIntercepts()
	check(t, rows, err)
	for _, query := range []struct {
		name string
		list func() ([]InterceptApprovalRow, error)
	}{
		{"task", func() ([]InterceptApprovalRow, error) { return d.ListTaskIntercepts(scope) }},
		{"global", func() ([]InterceptApprovalRow, error) { return d.ListAllIntercepts(10000) }},
		{"task page", func() ([]InterceptApprovalRow, error) {
			rows, _, err := d.ListTaskInterceptsPage(scope, 1, 100, InterceptApprovalFilter{})
			return rows, err
		}},
		{"global page", func() ([]InterceptApprovalRow, error) {
			rows, _, err := d.ListAllInterceptsPage(1, 100, InterceptApprovalFilter{Search: scope})
			return rows, err
		}},
	} {
		t.Run(query.name, func(t *testing.T) {
			rows, err := query.list()
			pending := make([]InterceptPending, 0, len(rows))
			for _, row := range rows {
				pending = append(pending, row.InterceptPending)
			}
			check(t, pending, err)
		})
	}
}

func TestInterceptPreExecutionCancellation(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	scope := "pre-execution-cancel-" + t.Name()
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM intercept_pending WHERE task_id=$1`, scope) })
	for _, byID := range []bool{false, true} {
		for _, status := range []string{"awaiting_result", "failed", "unknown", "succeeded"} {
			t.Run(fmt.Sprintf("id_%t/%s", byID, status), func(t *testing.T) {
				runID := scope + t.Name()
				audit := &InterceptAudit{RunID: runID, ToolUseID: "fixture-call", Correlation: "exact", EffectiveAction: "allow", ExecutionStatus: status}
				id, err := d.CreateDecidedIntercept(0, 0, scope, "mainagent", "Write", []byte(`{}`), "allowed", "fixture", audit)
				if err != nil {
					t.Fatal(err)
				}
				// Another run using the same tool ID must remain untouched.
				audit.RunID = runID + "-other"
				otherID, err := d.CreateDecidedIntercept(0, 0, scope, "planner", "Write", []byte(`{}`), "allowed", "fixture", audit)
				if err != nil {
					t.Fatal(err)
				}
				selectedID := int64(0)
				if byID {
					selectedID = id
				}
				if err := d.MarkInterceptNotExecuted(selectedID, runID, "fixture-call"); err != nil {
					t.Fatal(err)
				}
				// Synthetic results arriving later must not overwrite non-execution.
				if err := d.CompleteIntercept(id, runID, "fixture-call", "failed", "synthetic cancellation", false); err != nil {
					t.Fatal(err)
				}
				want := "not_executed"
				if status == "succeeded" {
					want = status
				}
				row, err := d.GetInterceptDetail(id)
				if err != nil || row == nil || row.Audit == nil || row.Status != "allowed" || row.Audit.ExecutionStatus != want {
					t.Fatalf("cancelled call=%+v want=%s err=%v", row, want, err)
				}
				other, err := d.GetInterceptDetail(otherID)
				if err != nil || other == nil || other.Audit == nil || other.Audit.ExecutionStatus != status {
					t.Fatalf("unrelated call changed=%+v err=%v", other, err)
				}
			})
		}
	}
}

func TestMainSessionOwnershipValidation(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	stores := make([]*ExplorationStore, 0, 2)
	for range 2 {
		task, err := d.CreateTask("main session validation", "fixture", nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = d.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID)
			_, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, task.ExplorationID)
		})
		stores = append(stores, d.Exploration(task.ExplorationID))
	}
	for range 3 {
		if _, err := stores[0].NewMainSession(); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		store *ExplorationStore
		seg   int
		want  bool
	}{
		{stores[0], -1, false}, {stores[0], 0, true}, {stores[0], 1, true},
		{stores[0], 3, true}, {stores[0], 4, false},
		{stores[1], 0, true}, {stores[1], 3, false},
	} {
		got, err := tc.store.HasMainSession(tc.seg)
		if err != nil || got != tc.want {
			t.Fatalf("segment=%d exists=%t want=%t err=%v", tc.seg, got, tc.want, err)
		}
	}
}
