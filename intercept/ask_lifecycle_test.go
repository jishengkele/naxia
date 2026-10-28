package intercept

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Hold channel registration behind the committed pending row so an HTTP/polling
// decision deterministically wins the INSERT-to-registration race.
func TestHandleAskEarlyDecisionKeepsActivityAndCancellation(t *testing.T) {
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("ARTEX_PG_DSN is required for PostgreSQL approval lifecycle tests")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for _, allow := range []bool{false, true} {
		for _, cancelRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("allow_%t/cancel_%t", allow, cancelRun), func(t *testing.T) {
				ic := New(d)
				scope := "early-approval-" + t.Name()
				t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM intercept_pending WHERE task_id=$1`, scope) })
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				activity := make(chan db.Activity, 1)
				ctx = WithTaskContext(ctx, scope, "mainagent", func(a db.Activity) { activity <- a })
				ctx = WithTaskSession(ctx, "main:3")
				ctx, trace := WithTrace(ctx, "approve local fixture", nil)
				defer trace.Finish()
				input := []byte(`{}`)
				trace.Start("early-call", "Write", input)
				ctx = WithCall(ctx, "Write", input)
				ic.pending.mu.Lock()
				locked := true
				defer func() {
					if locked {
						ic.pending.mu.Unlock()
					}
				}()
				result := make(chan bool, 1)
				go func() { result <- ic.HandleAsk(ctx, 0, Decision{Action: "ask"}, "Write", input) }()
				wait := func(check func() bool) {
					t.Helper()
					for !check() {
						select {
						case <-ctx.Done():
							t.Fatal("approval lifecycle timed out")
						case <-time.After(time.Millisecond):
						}
					}
				}
				var id int64
				wait(func() bool {
					return d.QueryRow(`SELECT id FROM intercept_pending WHERE task_id=$1`, scope).Scan(&id) == nil
				})
				decided := make(chan error, 1)
				go func() { decided <- ic.Decide(id, allow) }()
				wait(func() bool {
					row, err := d.GetInterceptPending(id)
					return err == nil && row != nil && row.Status != "pending"
				})
				if cancelRun {
					cancel()
				}
				locked = false
				ic.pending.mu.Unlock()
				select {
				case got := <-result:
					if got != (allow && !cancelRun) {
						t.Errorf("approved=%t allow=%t cancelled=%t", got, allow, cancelRun)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("early decision was lost")
				}
				if err := <-decided; err != nil {
					t.Fatal(err)
				}
				select {
				case a := <-activity:
					var detail struct {
						ID int64 `json:"pending_id"`
					}
					if a.Kind != "intercept_request" || json.Unmarshal([]byte(a.Detail), &detail) != nil || detail.ID != id {
						t.Fatalf("wrong approval activity: %+v", a)
					}
				default:
					t.Error("fast decision dropped the inline approval activity")
				}
				if allow && cancelRun {
					row, err := d.GetInterceptDetail(id)
					if err != nil || row == nil || row.Status != "allowed" || row.Audit == nil || row.Audit.ExecutionStatus != "not_executed" {
						t.Fatalf("cancelled approved call audit=%+v err=%v", row, err)
					}
				}
			})
		}
	}
}
