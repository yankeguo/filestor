package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	pushTimeLayout    = "2006-01-02T15:04"
	pushTitleMaxRunes = 80
)

var errInvalidPushTitle = errors.New("invalid title")

// pushState is the JSON-visible snapshot of a push job.
type pushState struct {
	Running    bool   `json:"running"`
	Prefix     string `json:"prefix"`
	Total      int    `json:"total"`
	Done       int    `json:"done"`
	TotalBytes int64  `json:"total_bytes"`
	DoneBytes  int64  `json:"done_bytes"`
	Current    string `json:"current"`
	Error      string `json:"error"`
}

type pushJob struct {
	mu sync.Mutex
	st pushState
}

func (j *pushJob) update(fn func(*pushState)) pushState {
	j.mu.Lock()
	defer j.mu.Unlock()
	fn(&j.st)
	return j.st
}

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

// pushPrefix builds YYYY/MM/YYYYMMDDhhmm-TITLE from the picked wall-clock time.
func pushPrefix(t time.Time, title string) string {
	return t.Format("2006/01/200601021504") + "-" + title
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
	prefix := pushPrefix(when, title)
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
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name)
	}
	job := &pushJob{}
	job.st = pushState{Running: true, Prefix: prefix + "/", Total: len(names)}
	s.emitProgress(jobFromPush(job.st), false)
	go s.runPush(job, dir, prefix, names)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "prefix": prefix + "/"})
}

func jobFromPush(st pushState) jobProgress {
	return jobProgress{
		Kind:       lockPush,
		File:       st.Current,
		Done:       st.Done,
		Total:      st.Total,
		DoneBytes:  st.DoneBytes,
		TotalBytes: st.TotalBytes,
		Prefix:     st.Prefix,
		Error:      st.Error,
	}
}

// runPush uploads every staged file to OSS under prefix, removing each file
// from the workspace once it lands. The first failure stops the job and keeps
// the remaining files staged.
func (s *Server) runPush(job *pushJob, dir, prefix string, names []string) {
	defer s.release(lockPush)
	log.Printf("push started: %s (%d files)", prefix, len(names))
	sizes := make([]int64, len(names))
	var totalBytes int64
	for i, name := range names {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
			sizes[i] = info.Size()
			totalBytes += info.Size()
		}
	}
	s.emitProgress(jobFromPush(job.update(func(st *pushState) { st.TotalBytes = totalBytes })), false)
	for _, name := range names {
		s.emitProgress(jobFromPush(job.update(func(st *pushState) { st.Current = name })), false)
		if err := s.pushOne(job, filepath.Join(dir, name), prefix+"/"+name); err != nil {
			log.Println("push:", err)
			st := job.update(func(st *pushState) {
				st.Running = false
				st.Error = fmt.Sprintf("%s: %v", name, err)
			})
			s.emitFail(jobFromPush(st))
			s.emitFiles()
			return
		}
		_ = os.Remove(filepath.Join(dir, name))
		s.emitProgress(jobFromPush(job.update(func(st *pushState) { st.Done++ })), false)
		s.emitFiles()
	}
	clearWorkspaceStateIfEmpty(dir)
	st := job.update(func(st *pushState) {
		st.Running = false
		st.Current = ""
	})
	s.emitDone(jobFromPush(st))
	s.emitState()
	log.Printf("push finished: %s", prefix)
}

func (s *Server) pushOne(job *pushJob, localPath, key string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	pr := &progressReader{r: f, on: func(n int64) {
		s.emitProgress(jobFromPush(job.update(func(st *pushState) { st.DoneBytes += n })), true)
	}}
	return s.store.Put(key, pr)
}
