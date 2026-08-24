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
)

const (
	uploadMaxBytes   = 2 << 30
	uploadMaxMemory  = 32 << 20
	uploadTempPrefix = ".upload-"
	uploadFilesField = "file"
	uploadNameQuery  = "name"
)

var errInvalidWorkspaceName = errors.New("invalid file name")

type workspaceFile struct {
	Name     string `json:"name"`
	Size     string `json:"size"`
	Modified string `json:"modified"`
}

type uploadPageData struct {
	Nav       string
	Workspace string
	Files     []workspaceFile
}

func (s *Server) workspaceDir() string {
	if s.Config != nil && strings.TrimSpace(s.Config.Upload.Workspace) != "" {
		return s.Config.Upload.Workspace
	}
	return defaultUploadWorkspace
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
	if strings.HasPrefix(name, uploadTempPrefix) {
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
		if e.IsDir() || strings.HasPrefix(e.Name(), uploadTempPrefix) {
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, uploadTempPrefix+"*")
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

func (s *Server) handleUploadPage(w http.ResponseWriter, r *http.Request) {
	files, err := listWorkspaceFiles(s.workspaceDir())
	if err != nil {
		log.Println("list workspace:", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	s.render(w, "upload.html", uploadPageData{
		Nav:       "upload",
		Workspace: s.workspaceDir(),
		Files:     files,
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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(map[string]any{"files": files}); err != nil {
		log.Println("encode workspace list:", err)
	}
}

func (s *Server) handleUploadAdd(w http.ResponseWriter, r *http.Request) {
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
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
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
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *Server) handleUploadDelete(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get(uploadNameQuery)
	if _, err := sanitizeWorkspaceName(name); err != nil {
		http.Error(w, "invalid file name", http.StatusBadRequest)
		return
	}
	if err := removeWorkspaceFile(s.workspaceDir(), name); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		log.Println("remove workspace file:", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(`{"ok":true}`))
}
