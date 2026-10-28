package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestArchiveEvidenceBodyCannotChangeAfterPreverification(t *testing.T) {
	original := []byte{0, 255, 128, 10}
	sum := sha256.Sum256(original)
	hash := hex.EncodeToString(sum[:])
	for _, body := range [][]byte{original, []byte{0, 255, 129, 10}, append(append([]byte{}, original...), 42), original[:2]} {
		var out bytes.Buffer
		err := copySnapshotBody(&out, bytes.NewReader(body), hash, int64(len(original)))
		if bytes.Equal(body, original) {
			if err != nil || !bytes.Equal(out.Bytes(), original) {
				t.Fatalf("complete original failed: %v", err)
			}
		} else if err == nil {
			t.Fatalf("changed evidence accepted: %v", body)
		}
	}
}
