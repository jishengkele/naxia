package traffic

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrafficDirectorySnapshotCutoffAndBinaryLegacyRoundTrip(t *testing.T) {
	tr, err := Open(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	const host = "directory.example"
	cutoff := time.Now()
	for _, row := range []struct {
		id string
		ts int64
	}{{"before", cutoff.Add(-time.Second).UnixMilli()}, {"after", cutoff.Add(time.Second).UnixMilli()}} {
		if _, err = tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path) VALUES(?,?,?,'GET','/','https://directory.example/',200,'binary',4,4,?)`, row.id, row.ts, host, row.id); err != nil {
			t.Fatal(err)
		}
		if err = os.Mkdir(filepath.Join(tr.dir, row.id), 0700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"request.http", "response.http"} {
			if err = os.WriteFile(filepath.Join(tr.dir, row.id, name), []byte{0, 255, 128, 10}, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	estimate, err := tr.SnapshotHostsBefore([]string{host}, cutoff, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "traffic")
	actual, err := tr.SnapshotHostsBefore([]string{host}, cutoff, dir)
	if err != nil {
		t.Fatal(err)
	}
	if actual != estimate || actual.Exchanges != 1 {
		t.Fatalf("estimate=%+v actual=%+v", estimate, actual)
	}
	imported, err := Open(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer imported.Close()
	count, err := imported.ImportArchive(dir)
	if err != nil || count != 1 {
		t.Fatalf("import count=%d err=%v", count, err)
	}
	for _, name := range []string{"request.http", "response.http"} {
		raw, err := os.ReadFile(filepath.Join(imported.dir, "before", name))
		if err != nil || !bytes.Equal(raw, []byte{0, 255, 128, 10}) {
			t.Fatalf("legacy raw bytes changed: %v %v", raw, err)
		}
	}
	req, resp, err := imported.Get("before")
	if err != nil || !bytes.Equal([]byte(req), []byte{0, 255, 128, 10}) || !bytes.Equal([]byte(resp), []byte{0, 255, 128, 10}) {
		t.Fatalf("legacy reader changed raw bytes: %v %v %v", []byte(req), []byte(resp), err)
	}
	var after int
	if err = imported.DB().QueryRow(`SELECT count(*) FROM exchanges WHERE id='after'`).Scan(&after); err != nil || after != 0 {
		t.Fatalf("post-cutoff record imported: %d %v", after, err)
	}
	if err = tr.DB().QueryRow(`SELECT count(*) FROM exchanges WHERE id='after'`).Scan(&after); err != nil || after != 1 {
		t.Fatalf("post-cutoff original removed: %d %v", after, err)
	}
}

func TestTrafficDirectoryBlobCopyIsAtomicAndRejectsExistingCorruption(t *testing.T) {
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	data := []byte("complete raw body")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyArchiveBlob(source, destination, strings.Repeat("0", 64)); err == nil {
		t.Fatal("bad source digest accepted")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("partial destination was published: %v", err)
	}
	if err := copyArchiveBlob(source, destination, hash); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyArchiveBlob(source, destination, hash); err == nil {
		t.Fatal("existing corrupt body accepted on retry")
	}
}

func TestTrafficDirectorySnapshotMissingBodyFailsInsteadOfSkipping(t *testing.T) {
	tr, err := Open(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if _, err = tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path) VALUES('missing',1,'missing.example','GET','/','/',200,'text/plain',1,1,'missing')`); err != nil {
		t.Fatal(err)
	}
	if _, err = tr.SnapshotHostsBefore([]string{"missing.example"}, time.Now(), ""); err == nil {
		t.Fatal("missing legacy body silently omitted")
	}
}
