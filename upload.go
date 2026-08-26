package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	uploadMaxBytes   = 2 << 30
	uploadMaxMemory  = 32 << 20
	uploadFilesField = "file"
	uploadNameQuery  = "name"

	// workspaceMetaDir collects every non-staged file (draft state, atomic
	// write temp files, conversion cache, analyze products) under one
	// dot-prefixed directory, so the workspace root only ever holds staged
	// files.
	workspaceMetaDir   = ".filestor"
	workspaceStateFile = "state.json"
	workspaceTmpDir    = "tmp"
	workspaceCacheDir  = "cache"
	workspaceAnalyze   = "analyze"
)

var errInvalidWorkspaceName = errors.New("invalid file name")

type workspaceFile struct {
	Name     string `json:"name"`
	Size     string `json:"size"`
	Modified string `json:"modified"`
}

type uploadPageData struct {
	Nav        string
	Workspace  string
	Files      []workspaceFile
	Time       string
	Title      string
	CanAnalyze bool
	Analyzed   bool
}

// workspaceState is the draft push options persisted as state.json inside the
// workspace's .filestor meta directory, so it survives page reloads but stays
// invisible to the staging list. Analyzed marks that the staged files went
// through one successful analyze run; adding or deleting staged files clears
// it, and push requires it while llm.chat.url is configured.
type workspaceState struct {
	Time     string `json:"time"`
	Title    string `json:"title"`
	Analyzed bool   `json:"analyzed"`
}

func (s *Server) workspaceDir() string {
	if s.Config != nil && strings.TrimSpace(s.Config.Upload.Workspace) != "" {
		return s.Config.Upload.Workspace
	}
	return defaultUploadWorkspace
}

func workspaceStatePath(dir string) string {
	return filepath.Join(dir, workspaceMetaDir, workspaceStateFile)
}

func workspaceTmpPath(dir string) string {
	return filepath.Join(dir, workspaceMetaDir, workspaceTmpDir)
}

func workspaceCachePath(dir string) string {
	return filepath.Join(dir, workspaceMetaDir, workspaceCacheDir)
}

// workspaceAnalyzePath is the per-run directory of derived, model-readable
// forms (hard links into the conversion cache), rebuilt by every analyze run.
func workspaceAnalyzePath(dir string) string {
	return filepath.Join(dir, workspaceMetaDir, workspaceAnalyze)
}

// prepWorkspace creates the .filestor meta directory and removes stale temp
// files from interrupted writes, expired conversion cache entries, and the
// previous run's analyze products.
func prepWorkspace(dir string) error {
	if err := os.MkdirAll(workspaceTmpPath(dir), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(workspaceCachePath(dir), 0o755); err != nil {
		return err
	}
	if entries, err := os.ReadDir(workspaceTmpPath(dir)); err == nil {
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(workspaceTmpPath(dir), e.Name()))
		}
	}
	if err := resetAnalyzeDir(dir); err != nil {
		return err
	}
	pruneConvertCache(dir)
	return nil
}

// resetAnalyzeDir clears and recreates the analyze products directory.
func resetAnalyzeDir(dir string) error {
	if err := os.RemoveAll(workspaceAnalyzePath(dir)); err != nil {
		return err
	}
	return os.MkdirAll(workspaceAnalyzePath(dir), 0o755)
}

func sanitizeWorkspaceName(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	if name == "" || name == "." || name == ".." {
		return "", errInvalidWorkspaceName
	}
	if strings.ContainsAny(name, `/\:`) || strings.ContainsRune(name, 0) {
		return "", errInvalidWorkspaceName
	}
	// Reject dot-prefixed hidden files (covers the .filestor meta dir too).
	if strings.HasPrefix(name, ".") {
		return "", errInvalidWorkspaceName
	}
	return name, nil
}

func listWorkspaceFiles(dir string) ([]workspaceFile, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]workspaceFile, 0, len(entries))
	for _, e := range entries {
		// Skip subdirectories and dot-prefixed hidden files (incl. the
		// .filestor meta directory).
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		out = append(out, workspaceFile{
			Name:     e.Name(),
			Size:     formatSize(info.Size()),
			Modified: formatTime(info.ModTime()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func saveWorkspaceFile(dir, name string, r io.Reader) error {
	name, err := sanitizeWorkspaceName(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(workspaceTmpPath(dir), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(workspaceTmpPath(dir), "tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	dest := filepath.Join(dir, name)
	_ = os.Remove(dest)
	return os.Rename(tmpName, dest)
}

func removeWorkspaceFile(dir, name string) error {
	name, err := sanitizeWorkspaceName(name)
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, name))
}

// renameWorkspaceFile renames a staged file, refusing to overwrite a different
// existing file. A target that stats to the same file (a case-only change on a
// case-insensitive filesystem) is allowed through.
func renameWorkspaceFile(dir, oldName, newName string) error {
	oldName, err := sanitizeWorkspaceName(oldName)
	if err != nil {
		return err
	}
	newName, err = sanitizeWorkspaceName(newName)
	if err != nil {
		return err
	}
	if oldName == newName {
		return errors.New("new name equals old name")
	}
	src := filepath.Join(dir, oldName)
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	dst := filepath.Join(dir, newName)
	if dstInfo, err := os.Stat(dst); err == nil && !os.SameFile(srcInfo, dstInfo) {
		return errors.New("a file with the new name already exists")
	}
	return os.Rename(src, dst)
}

func loadWorkspaceState(dir string) workspaceState {
	var st workspaceState
	data, err := os.ReadFile(workspaceStatePath(dir))
	if err != nil {
		return st
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return workspaceState{}
	}
	return st
}

func saveWorkspaceState(dir string, st workspaceState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(workspaceTmpPath(dir), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(workspaceTmpPath(dir), "tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, workspaceStatePath(dir))
}

// workspaceStateStore keeps the draft push options in memory: the state file
// is loaded once at startup (like the monthly bundle index), reads hit memory,
// and every mutation writes through to state.json. The workspace lock
// serializes mutations, but PUT /upload/state and SSE snapshots only read, so
// the store carries its own mutex.
type workspaceStateStore struct {
	mu     sync.Mutex
	dir    string
	state  workspaceState
	exists bool // a state file was loaded at startup or saved since
}

func newWorkspaceStateStore(dir string) *workspaceStateStore {
	w := &workspaceStateStore{dir: dir}
	data, err := os.ReadFile(workspaceStatePath(dir))
	if err != nil {
		return w
	}
	var st workspaceState
	if err := json.Unmarshal(data, &st); err != nil {
		// A corrupt state file is treated as absent: an exists=true store
		// holding a zero state would make pin() a permanent no-op.
		log.Println("load workspace state:", err)
		return w
	}
	w.exists = true
	w.state = st
	return w
}

func (w *workspaceStateStore) get() workspaceState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

// save writes the file first and updates memory only once the write lands.
func (w *workspaceStateStore) save(st workspaceState) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := saveWorkspaceState(w.dir, st); err != nil {
		return err
	}
	w.state = st
	w.exists = true
	return nil
}

// clear removes the state file (staging emptied) and zeroes memory.
func (w *workspaceStateStore) clear() {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = os.Remove(workspaceStatePath(w.dir))
	w.state = workspaceState{}
	w.exists = false
}

// pin writes the first-file draft (time=now, title="") only when no state
// exists yet.
func (w *workspaceStateStore) pin() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.exists {
		return
	}
	st := workspaceState{Time: time.Now().Format(pushTimeLayout)}
	if err := saveWorkspaceState(w.dir, st); err != nil {
		log.Println("save workspace state:", err)
		return
	}
	w.state = st
	w.exists = true
}

// markUnanalyzed clears the analyzed flag after the staged file set changes
// (file added or deleted), so a push requires a fresh analyze run. It is a
// no-op when no state exists yet.
func (w *workspaceStateStore) markUnanalyzed() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.exists || !w.state.Analyzed {
		return
	}
	st := w.state
	st.Analyzed = false
	if err := saveWorkspaceState(w.dir, st); err != nil {
		log.Println("save workspace state:", err)
		return
	}
	w.state = st
}

// clearWorkspaceStateIfEmpty drops the draft state once staging is empty.
func (s *Server) clearWorkspaceStateIfEmpty() {
	files, err := listWorkspaceFiles(s.workspaceDir())
	if err == nil && len(files) == 0 {
		s.state.clear()
	}
}

func (s *Server) handleUploadPage(w http.ResponseWriter, r *http.Request) {
	dir := s.workspaceDir()
	files, err := listWorkspaceFiles(dir)
	if err != nil {
		log.Println("list workspace:", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	st := s.state.get()
	s.render(w, "upload.html", uploadPageData{
		Nav:        "upload",
		Workspace:  dir,
		Files:      files,
		Time:       st.Time,
		Title:      st.Title,
		CanAnalyze: s.Config != nil && s.Config.LLM.Chat.URL != "",
		Analyzed:   st.Analyzed,
	})
}

func (s *Server) handleUploadList(w http.ResponseWriter, r *http.Request) {
	files, err := listWorkspaceFiles(s.workspaceDir())
	if err != nil {
		log.Println("list workspace:", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	if files == nil {
		files = []workspaceFile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *Server) handleUploadAdd(w http.ResponseWriter, r *http.Request) {
	if !s.acquire(lockStage) {
		workspaceBusy(w)
		return
	}
	defer s.release(lockStage)
	r.Body = http.MaxBytesReader(w, r.Body, uploadMaxBytes)
	if err := r.ParseMultipartForm(uploadMaxMemory); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad multipart", http.StatusBadRequest)
		return
	}
	if r.MultipartForm == nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	headers := r.MultipartForm.File[uploadFilesField]
	if len(headers) == 0 {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	dir := s.workspaceDir()
	for _, fh := range headers {
		name, err := sanitizeWorkspaceName(fh.Filename)
		if err != nil {
			http.Error(w, "invalid file name", http.StatusBadRequest)
			return
		}
		src, err := fh.Open()
		if err != nil {
			http.Error(w, "read file", http.StatusBadRequest)
			return
		}
		err = saveWorkspaceFile(dir, name, src)
		_ = src.Close()
		if err != nil {
			log.Println("save workspace file:", err)
			http.Error(w, "save failed", http.StatusInternalServerError)
			return
		}
		// Pin as soon as the first file lands, even if a later part fails.
		s.state.pin()
	}
	// New staged files invalidate the previous analyze run.
	s.state.markUnanalyzed()
	s.emitFiles()
	s.emitState()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUploadDelete(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get(uploadNameQuery)
	if _, err := sanitizeWorkspaceName(name); err != nil {
		http.Error(w, "invalid file name", http.StatusBadRequest)
		return
	}
	if !s.acquire(lockStage) {
		workspaceBusy(w)
		return
	}
	defer s.release(lockStage)
	dir := s.workspaceDir()
	if err := removeWorkspaceFile(dir, name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		log.Println("remove workspace file:", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	// A changed staging set invalidates the previous analyze run.
	s.state.markUnanalyzed()
	s.clearWorkspaceStateIfEmpty()
	s.emitFiles()
	s.emitState()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleUploadState persists the draft push time/title while files are staged.
func (s *Server) handleUploadState(w http.ResponseWriter, r *http.Request) {
	if s.busyForState() {
		workspaceBusy(w)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	dir := s.workspaceDir()
	st := s.state.get()
	if v := strings.TrimSpace(r.Form.Get("time")); v != "" {
		if _, err := time.Parse(pushTimeLayout, v); err != nil {
			http.Error(w, "invalid time", http.StatusBadRequest)
			return
		}
		st.Time = v
	}
	st.Title = strings.TrimSpace(r.Form.Get("title"))
	files, err := listWorkspaceFiles(dir)
	if err != nil {
		log.Println("list workspace:", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	// Nothing staged: keep the no-op a success, but do not pin options yet.
	if len(files) > 0 {
		if err := s.state.save(st); err != nil {
			log.Println("save workspace state:", err)
			http.Error(w, "save failed", http.StatusInternalServerError)
			return
		}
		s.emitState()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
