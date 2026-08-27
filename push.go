package main

import (
	"bytes"
	"context"
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
	if s.Config != nil && s.Config.LLM.Chat.OpenAI.URL != "" && !s.state.get().Analyzed {
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

// runPush uploads every staged file to the bucket under prefix, then the
// analyze run's digest marks into the bundle's .digest directory, then
// .meta.json, and finally records the bundle in the monthly index (rewriting
// the bucket file and updating the in-memory copy). .meta.json is written
// last so an interrupted job leaves no discoverable bundle behind. Staged
// files are only removed from the workspace once the index write lands, so a
// failed push leaves the whole batch staged for a retry instead of orphaning
// the bundle. The first failure stops the job and keeps everything staged.
// job is owned by this goroutine alone (the progressReader callback runs
// inside store.Put on the same goroutine), so it needs no locking.
func (s *Server) runPush(job jobProgress, dir, prefix string, names []string, meta bundleMeta) {
	defer s.release(lockPush)
	defer func() {
		if r := recover(); r != nil {
			log.Println("push panic:", r)
			s.emitFail(jobProgress{Kind: lockPush, Error: "internal error"})
		}
	}()
	log.Printf("push started: %s (%d files)", prefix, len(names))
	for _, name := range names {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
			job.TotalBytes += info.Size()
		}
	}
	// The analyze run's digest marks ride along into the bundle's .digest
	// directory; a missing directory just means there was nothing to mark.
	var digest []string
	if entries, err := os.ReadDir(workspaceDigestPath(dir)); err == nil {
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			digest = append(digest, e.Name())
			job.TotalBytes += info.Size()
		}
	}
	job.Total += len(digest)
	s.emitProgress(job, false)
	raw, err := json.Marshal(meta)
	if err != nil {
		job.Error = fmt.Sprintf("%v (bundle %s)", err, prefix)
		s.emitFail(job)
		return
	}
	fail := func(name string, err error) {
		log.Println("push:", err)
		job.Error = fmt.Sprintf("%s: %v (bundle %s)", name, err, prefix)
		s.emitFail(job)
		s.emitFiles()
	}
	for _, name := range names {
		job.File = name
		s.emitProgress(job, false)
		if err := s.pushOne(&job, filepath.Join(dir, name), prefix+"/"+name); err != nil {
			fail(name, err)
			return
		}
		job.Done++
		s.emitProgress(job, false)
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
			log.Printf("push %s: uploaded %s (%s)", prefix, name, formatSize(info.Size()))
		}
	}
	for _, name := range digest {
		job.File = bundleDigestDir + "/" + name
		s.emitProgress(job, false)
		if err := s.pushOne(&job, filepath.Join(workspaceDigestPath(dir), name), prefix+"/"+bundleDigestDir+"/"+name); err != nil {
			fail(job.File, err)
			return
		}
		job.Done++
		s.emitProgress(job, false)
		log.Printf("push %s: uploaded %s", prefix, job.File)
	}
	// Embed the digest marks and write the vectors before the bundle becomes
	// discoverable (meta) and recorded (index): a failure here stops the job
	// with everything still staged for a retry.
	if len(digest) > 0 && s.embeddingConfigured() {
		ec := newEmbedClient(s.Config.LLM.Embeddings.BailianMultimodalEmbedding)
		vecs, err := ec.embedDigest(context.Background(), workspaceDigestPath(dir), digest, func(i int, name string) {
			job.Message = fmt.Sprintf("embedding digest %d/%d", i+1, len(digest))
			s.emitProgress(job, false)
		})
		if err != nil {
			fail("embed", err)
			return
		}
		vc, err := newVectorsClient(s.Config.LLM.Vectors.AliyunOSSVectors)
		if err != nil {
			fail("vectors", err)
			return
		}
		if err := vc.putVectors(context.Background(), meta.ID, meta, digest, vecs); err != nil {
			fail("vectors", err)
			return
		}
		job.Message = ""
	} else if len(digest) == 0 {
		log.Printf("push %s: no digest marks, skipping embedding", prefix)
	} else {
		log.Printf("push %s: embedding pipeline not configured, skipping %d digest marks", prefix, len(digest))
	}
	// The bundle is complete: publish its meta, then record it in the index.
	if err := s.store.Put(prefix+"/"+bundleMetaName, bytes.NewReader(raw), int64(len(raw))); err != nil {
		fail(bundleMetaName, err)
		return
	}
	log.Printf("push %s: %s published", prefix, bundleMetaName)
	if err := s.index.append(s.store, meta); err != nil {
		log.Println("push index:", err)
		job.Error = fmt.Sprintf("index: %v (bundle %s)", err, prefix)
		s.emitFail(job)
		s.emitFiles()
		return
	}
	// The bundle is recorded; drop the staged copies. A failed delete only
	// leaves a duplicate behind, so it is logged, not fatal.
	removed := 0
	for _, name := range names {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			log.Println("remove staged file:", err)
		} else {
			removed++
		}
	}
	log.Printf("push %s: removed %d/%d staged files", prefix, removed, len(names))
	s.clearWorkspaceStateIfEmpty()
	job.File = ""
	s.emitFiles()
	s.emitDone(job)
	s.emitState()
	log.Printf("push finished: %s", prefix)
}

// embeddingConfigured reports whether the digest embedding pipeline is fully
// configured: both the embeddings endpoint and the vector store.
func (s *Server) embeddingConfigured() bool {
	if s.Config == nil {
		return false
	}
	e := s.Config.LLM.Embeddings.BailianMultimodalEmbedding
	v := s.Config.LLM.Vectors.AliyunOSSVectors
	return e.URL != "" && v.URL != ""
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
