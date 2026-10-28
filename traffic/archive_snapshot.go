package traffic

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ArchiveEstimate struct {
	Bytes     int64  `json:"bytes"`
	Files     int64  `json:"files"`
	Exchanges int64  `json:"exchanges"`
	Digest    string `json:"digest"`
}

type archiveCountingWriter struct {
	n      int64
	writer io.Writer
}

func (w *archiveCountingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.n += int64(n)
	return n, err
}

// A fixed timestamp excludes later captures; both estimation and export visit
// identical rows. The writer lock keeps each exchange and its body consistent.
// Estimation never creates a payload or loads the entire traffic set in memory.
func (t *Traffic) SnapshotHostsBefore(hosts []string, cutoff time.Time, dir string) (ArchiveEstimate, error) {
	var result ArchiveEstimate
	if t == nil || len(hosts) == 0 {
		return result, nil
	}
	t.wmu.Lock()
	defer t.wmu.Unlock()
	unique := uniqueArchiveHosts(hosts)
	if len(unique) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(unique))
	args := make([]any, len(unique))
	for i, host := range unique {
		placeholders[i] = "?"
		args[i] = host
	}
	args = append(args, cutoff.UnixMilli())
	rows, err := t.db.Query(`SELECT id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path FROM exchanges WHERE host IN (`+strings.Join(placeholders, ",")+`) AND ts<=? ORDER BY ts,id`, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	var output io.Writer = io.Discard
	var file *os.File
	if dir != "" {
		if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0700); err != nil {
			return result, err
		}
		file, err = os.OpenFile(filepath.Join(dir, "exchanges.ndjson"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return result, err
		}
		defer file.Close()
		output = file
	}
	h := sha256.New()
	counter := &archiveCountingWriter{writer: io.MultiWriter(output, h)}
	encoder := json.NewEncoder(counter)
	blobs := map[string]bool{}
	for rows.Next() {
		var item ArchiveExchange
		var legacy string
		if err := rows.Scan(&item.ID, &item.TS, &item.Host, &item.Method, &item.URLTemplate, &item.URL, &item.Status, &item.ContentType, &item.ReqLen, &item.RespLen, &legacy); err != nil {
			return result, err
		}
		var reqBlob, respBlob sql.NullString
		err := t.db.QueryRow(`SELECT req_head,req_body,req_blob,resp_head,resp_body,resp_blob FROM exchange_bodies WHERE id=?`, item.ID).Scan(&item.ReqHead, &item.ReqBody, &reqBlob, &item.RespHead, &item.RespBody, &respBlob)
		if errors.Is(err, sql.ErrNoRows) && legacy != "" {
			clean := filepath.Clean(legacy)
			if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
				return result, fmt.Errorf("invalid legacy traffic path %q", legacy)
			}
			root, err := os.OpenRoot(t.dir)
			if err != nil {
				return result, err
			}
			item.LegacyRequest, err = root.ReadFile(filepath.Join(clean, "request.http"))
			if err == nil {
				item.LegacyResponse, err = root.ReadFile(filepath.Join(clean, "response.http"))
			}
			root.Close()
			if err != nil {
				return result, fmt.Errorf("流量 %s 正文缺失: %w", item.ID, err)
			}
			item.LegacyPath = clean
			item.ReqHead = string(item.LegacyRequest)
			item.RespHead = string(item.LegacyResponse)
		} else if err != nil {
			return result, fmt.Errorf("流量 %s 正文缺失: %w", item.ID, err)
		}
		item.ReqBlob, item.RespBlob = reqBlob.String, respBlob.String
		for _, hash := range []string{item.ReqBlob, item.RespBlob} {
			if hash != "" {
				blobs[hash] = true
			}
		}
		if err := encoder.Encode(item); err != nil {
			return result, err
		}
		result.Exchanges++
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if file != nil {
		if err := file.Sync(); err != nil {
			return result, err
		}
	}
	result.Bytes = counter.n
	result.Files = 2
	result.Digest = hex.EncodeToString(h.Sum(nil))
	var blobNames []string
	for hash := range blobs {
		path, err := t.blobPath(hash)
		if err != nil {
			return result, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return result, fmt.Errorf("流量正文 %s 缺失: %w", hash, err)
		}
		result.Bytes += info.Size()
		result.Files++
		blobNames = append(blobNames, hash)
		if dir != "" {
			if err := copyArchiveBlob(path, filepath.Join(dir, "blobs", hash+".bin"), hash); err != nil {
				return result, err
			}
		}
	}
	sort.Strings(blobNames)
	meta := ArchiveSnapshot{Version: 2, RowsPath: "exchanges.ndjson", Blobs: blobNames}
	raw, err := json.Marshal(meta)
	if err != nil {
		return result, err
	}
	result.Bytes += int64(len(raw))
	if dir != "" {
		metaFile, err := os.OpenFile(filepath.Join(dir, "traffic.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return result, err
		}
		_, writeErr := metaFile.Write(raw)
		if err := errors.Join(writeErr, metaFile.Sync(), metaFile.Close()); err != nil {
			return result, err
		}
	}
	return result, nil
}

func copyArchiveBlob(source, destination, hash string) error {
	if _, err := os.Lstat(destination); err == nil {
		f, err := os.Open(destination)
		if err != nil {
			return err
		}
		digest := sha256.New()
		_, readErr := io.Copy(digest, f)
		if err = errors.Join(readErr, f.Close()); err != nil {
			return err
		}
		if hex.EncodeToString(digest.Sum(nil)) != hash {
			return fmt.Errorf("现有流量正文损坏或冲突: %s", hash)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(destination), ".archive-blob-")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), in)
	err = errors.Join(err, out.Sync(), out.Close())
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != hash {
		return fmt.Errorf("流量正文哈希不匹配: %s", hash)
	}
	return os.Rename(out.Name(), destination)
}

func writeLegacyArchiveFile(root *os.Root, name string, data []byte) error {
	if existing, err := root.ReadFile(name); err == nil {
		if string(existing) != string(data) {
			return fmt.Errorf("legacy traffic restore conflict: %s", name)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	temporary := name + ".restore-tmp"
	f, err := root.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	_, writeErr := f.Write(data)
	if err = errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	return root.Rename(temporary, name)
}
