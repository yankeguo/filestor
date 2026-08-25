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
	"time"
)

const (
	uploadMaxBytes   = 2 << 30
	uploadMaxMemory  = 32 << 20
	uploadFilesField = "file"
	uploadNameQuery  = "name"

	// workspaceMetaDir collects every non-staged file (draft state, atomic
	// write temp files, conversion cache) under one dot-prefixed directory,
	// so the workspace root only ever holds staged files.
	workspaceMetaDir   = ".filestor"
	workspaceStateFile = "state.json"
	workspaceTmpDir    = "tmp"
	workspaceCacheDir  = "cache"
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
// it, and push requires it while llm.url is configured.
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

// prepWorkspace creates the .filestor meta directory and removes stale temp
// files from interrupted writes and expired conversion cache entries.
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
	pruneConvertCache(dir)
	return nil
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

func clearWorkspaceState(dir string) {
	_ = os.Remove(workspaceStatePath(dir))
}

// pinWorkspaceState writes the first-file draft (time=now, title="") only when
// no state file exists yet.
func pinWorkspaceState(dir string) {
	if _, err := os.Stat(workspaceStatePath(dir)); !errors.Is(err, os.ErrNotExist) {
		return
	}
	if err := saveWorkspaceState(dir, workspaceState{Time: time.Now().Format(pushTimeLayout)}); err != nil {
		log.Println("save workspace state:", err)
	}
}

// markWorkspaceUnanalyzed clears the analyzed flag after the staged file set
// changes (file added or deleted), so a push requires a fresh analyze run.
// It is a no-op when no state file exists yet.
func markWorkspaceUnanalyzed(dir string) {
	if _, err := os.Stat(workspaceStatePath(dir)); errors.Is(err, os.ErrNotExist) {
		return
	}
	st := loadWorkspaceState(dir)
	if !st.Analyzed {
		return
	}
	st.Analyzed = false
	if err := saveWorkspaceState(dir, st); err != nil {
		log.Println("save workspace state:", err)
	}
}

func clearWorkspaceStateIfEmpty(dir string) {
	files, err := listWorkspaceFiles(dir)
	if err == nil && len(files) == 0 {
		clearWorkspaceState(dir)
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
	st := loadWorkspaceState(dir)
	s.render(w, "upload.html", uploadPageData{
		Nav:        "upload",
		Workspace:  dir,
		Files:      files,
		Time:       st.Time,
		Title:      st.Title,
		CanAnalyze: s.Config != nil && s.Config.LLM.URL != "",
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
		pinWorkspaceState(dir)
	}
	// New staged files invalidate the previous analyze run.
	markWorkspaceUnanalyzed(dir)
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
	markWorkspaceUnanalyzed(dir)
	clearWorkspaceStateIfEmpty(dir)
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
	st := loadWorkspaceState(dir)
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
		if err := saveWorkspaceState(dir, st); err != nil {
			log.Println("save workspace state:", err)
			http.Error(w, "save failed", http.StatusInternalServerError)
			return
		}
		s.emitState()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
