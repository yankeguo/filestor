package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	pushTimeLayout    = "2006-01-02T15:04"
	pushTitleMaxRunes = 80
)

var errInvalidPushTitle = errors.New("invalid title")

// progressReader reports every read to on, driving DoneBytes.
type progressReader struct {
	r  io.Reader
	on func(n int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.on(int64(n))
	}
	return n, err
}

// sanitizePushTitle keeps letters (incl. CJK), digits, '_', '.', and folds
// everything else (spaces, slashes, symbols) into single dashes.
func sanitizePushTitle(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errInvalidPushTitle
	}
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.':
			b.WriteRune(r)
			dash = false
		default:
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "", errInvalidPushTitle
	}
	if utf8.RuneCountInString(out) > pushTitleMaxRunes {
		out = strings.TrimRight(string([]rune(out)[:pushTitleMaxRunes]), "-.")
	}
	return out, nil
}

func (s *Server) handleUploadPush(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "object store unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	when, err := time.Parse(pushTimeLayout, r.Form.Get("time"))
	if err != nil {
		http.Error(w, "invalid time", http.StatusBadRequest)
		return
	}
	title, err := sanitizePushTitle(r.Form.Get("title"))
	if err != nil {
		http.Error(w, "invalid title", http.StatusBadRequest)
		return
	}
	id, err := newUUIDv4()
	if err != nil {
		log.Println("uuid:", err)
		http.Error(w, "id failed", http.StatusInternalServerError)
		return
	}
	prefix := bundlePrefix(id)
	meta := bundleMeta{ID: id, Title: title, Time: when.Format(pushTimeLayout)}
	dir := s.workspaceDir()
	files, err := listWorkspaceFiles(dir)
	if err != nil {
		log.Println("list workspace:", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	if len(files) == 0 {
		http.Error(w, "no staged files", http.StatusBadRequest)
		return
	}
	if !s.acquire(lockPush) {
		workspaceBusy(w)
		return
	}
	// With the LLM configured, staged files must go through one successful
	// analyze run before they can be pushed.
	if s.Config != nil && s.Config.LLM.URL != "" && !loadWorkspaceState(dir).Analyzed {
		s.release(lockPush)
		http.Error(w, "run analyze first", http.StatusBadRequest)
		return
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name)
	}
	job := jobProgress{Kind: lockPush, Prefix: prefix + "/", Total: len(names)}
	s.emitProgress(job, false)
	go s.runPush(job, dir, prefix, names, meta)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "prefix": prefix + "/", "id": id})
}

// runPush uploads .meta.json then every staged file to the bucket under prefix,
// removing each staged file from the workspace once it lands. After all files
// succeed it records the bundle in the monthly index (rewriting the bucket
// file and updating the in-memory copy). The first failure stops
// the job and keeps the remaining files staged. job is owned by this goroutine
// alone (the progressReader callback runs inside store.Put on the same
// goroutine), so it needs no locking.
func (s *Server) runPush(job jobProgress, dir, prefix string, names []string, meta bundleMeta) {
	defer s.release(lockPush)
	log.Printf("push started: %s (%d files)", prefix, len(names))
	for _, name := range names {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
			job.TotalBytes += info.Size()
		}
	}
	s.emitProgress(job, false)
	raw, err := json.Marshal(meta)
	if err != nil {
		job.Error = err.Error()
		s.emitFail(job)
		return
	}
	if err := s.store.Put(prefix+"/"+bundleMetaName, bytes.NewReader(raw), int64(len(raw))); err != nil {
		log.Println("push meta:", err)
		job.Error = fmt.Sprintf("%s: %v", bundleMetaName, err)
		s.emitFail(job)
		return
	}
	for _, name := range names {
		job.File = name
		s.emitProgress(job, false)
		if err := s.pushOne(&job, filepath.Join(dir, name), prefix+"/"+name); err != nil {
			log.Println("push:", err)
			job.Error = fmt.Sprintf("%s: %v", name, err)
			s.emitFail(job)
			s.emitFiles()
			return
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			log.Println("remove staged file:", err)
		}
		job.Done++
		s.emitProgress(job, false)
		s.emitFiles()
	}
	if err := s.index.append(s.store, meta); err != nil {
		log.Println("push index:", err)
		job.Error = fmt.Sprintf("index: %v", err)
		s.emitFail(job)
		s.emitFiles()
		return
	}
	clearWorkspaceStateIfEmpty(dir)
	job.File = ""
	s.emitDone(job)
	s.emitState()
	log.Printf("push finished: %s", prefix)
}

func (s *Server) pushOne(job *jobProgress, localPath, key string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	pr := &progressReader{r: f, on: func(n int64) {
		job.DoneBytes += n
		s.emitProgress(*job, true)
	}}
	return s.store.Put(key, pr, info.Size())
}
