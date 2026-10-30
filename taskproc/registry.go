// Package taskproc records cooperative process ownership. It is not a sandbox:
// children which discard their environment remain unknown archive blockers.
package taskproc

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const EnvironmentKey = "NAXIA_PROCESS_SCOPE"

type Scope struct {
	Token     string    `json:"token"`
	TaskID    int64     `json:"task_id"`
	IntentID  int64     `json:"intent_id"`
	SessionID string    `json:"session_id"`
	CreatedAt time.Time `json:"created_at"`
}

type Process struct {
	PID        int    `json:"pid"`
	StartTicks uint64 `json:"start_ticks"`
	Name       string `json:"name"`
	TaskID     int64  `json:"task_id"`
	SessionID  string `json:"session_id,omitempty"`
	IntentID   int64  `json:"intent_id,omitempty"`
	token      string
}

type Registry struct{ Root string }

// Register persists before exposing the marker to a child. Records deliberately
// outlive sessions and application restarts; a shell exiting is not proof that
// its descendants have exited. They live outside archived task directories.
func (r *Registry) Register(taskID, intentID int64, sessionID string) ([]string, error) {
	if taskID <= 0 {
		return nil, fmt.Errorf("invalid process task id")
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	s := Scope{Token: hex.EncodeToString(nonce[:]), TaskID: taskID, IntentID: intentID, SessionID: sessionID, CreatedAt: time.Now().UTC()}
	dir := filepath.Join(r.Root, strconv.FormatInt(taskID, 10))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, s.Token+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := syncDirectory(dir); err != nil {
		return nil, err
	}
	if err := syncDirectory(r.Root); err != nil {
		return nil, err
	}
	return []string{EnvironmentKey + "=" + s.Token}, nil
}

func (r *Registry) scopes(taskID int64) (map[string]Scope, error) {
	dir := filepath.Join(r.Root, strconv.FormatInt(taskID, 10))
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return map[string]Scope{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]Scope{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var s Scope
		if err = json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		decoded, err := hex.DecodeString(s.Token)
		if err != nil || len(decoded) != 32 || s.TaskID != taskID || e.Name() != s.Token+".json" {
			return nil, fmt.Errorf("invalid process ownership record %s", e.Name())
		}
		out[s.Token] = s
	}
	return out, nil
}
