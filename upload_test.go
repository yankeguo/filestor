package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func cfgWithWorkspace(t *testing.T) *Config {
	t.Helper()
	cfg := testCfg()
	cfg.Upload.Workspace = t.TempDir()
	return cfg
}

func TestSanitizeWorkspaceName(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"a.txt", "a.txt", false},
		{"  a.txt  ", "a.txt", false},
		{`dir\b.txt`, "b.txt", false},
		{"dir/b.txt", "b.txt", false},
		{"", "", true},
		{".", "", true},
		{"..", "", true},
		{"../secret", "secret", false},
		{".hidden", "", true},
		{".gitignore", "", true},
		{"dir/.hidden", "", true},
		{uploadTempPrefix + "x", "", true},
	}
	for _, tc := range cases {
		got, err := sanitizeWorkspaceName(tc.in)
		if tc.wantErr {
			require.Error(t, err, tc.in)
			continue
		}
		require.NoError(t, err, tc.in)
		require.Equal(t, tc.want, got, tc.in)
	}
}

func TestUploadRequiresLogin(t *testing.T) {
	h := NewServer(cfgWithWorkspace(t), &fakeStore{}).Handler()
	for _, path := range []string{"/upload", "/upload/files"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusFound, rec.Code, path)
		require.Equal(t, "/login", rec.Header().Get("Location"), path)
	}
}

func TestUploadPageAndList(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "readme.txt"), []byte("hi"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, ".hidden"), []byte("hi"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(cfg.Upload.Workspace, "subdir"), 0o755))

	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "readme.txt")
	require.NotContains(t, body, ".hidden")
	require.NotContains(t, body, "subdir")
	require.Contains(t, body, `href="/upload"`)
	require.Contains(t, body, "nav-link active")

	req = httptest.NewRequest(http.MethodGet, "/upload/files", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var payload struct {
		Files []workspaceFile `json:"files"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Len(t, payload.Files, 1)
	require.Equal(t, "readme.txt", payload.Files[0].Name)
}

func TestUploadListSeesExternalWrites(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/upload/files", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var payload struct {
		Files []workspaceFile `json:"files"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Empty(t, payload.Files)

	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "dropped.bin"), []byte("abc"), 0o644))

	req = httptest.NewRequest(http.MethodGet, "/upload/files", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Len(t, payload.Files, 1)
	require.Equal(t, "dropped.bin", payload.Files[0].Name)
}

func TestUploadAddAndDelete(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "docs/hello.txt")
	require.NoError(t, err)
	_, err = io.WriteString(part, "hello")
	require.NoError(t, err)
	part, err = mw.CreateFormFile("file", "second.txt")
	require.NoError(t, err)
	_, err = io.WriteString(part, "two")
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost, "/upload/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	raw, err := os.ReadFile(filepath.Join(cfg.Upload.Workspace, "hello.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello", string(raw))
	raw, err = os.ReadFile(filepath.Join(cfg.Upload.Workspace, "second.txt"))
	require.NoError(t, err)
	require.Equal(t, "two", string(raw))

	req = httptest.NewRequest(http.MethodDelete, "/upload/files?name=hello.txt", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	_, err = os.Stat(filepath.Join(cfg.Upload.Workspace, "hello.txt"))
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(cfg.Upload.Workspace, "second.txt"))
	require.NoError(t, err)
}

func TestUploadDeleteRejectsDotDot(t *testing.T) {
	h := NewServer(cfgWithWorkspace(t), &fakeStore{}).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodDelete, "/upload/files?name=..", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUploadDeleteMissing(t *testing.T) {
	h := NewServer(cfgWithWorkspace(t), &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	req := httptest.NewRequest(http.MethodDelete, "/upload/files?name=nope.txt", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestUploadAddRejectsHidden(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", ".gitignore")
	require.NoError(t, err)
	_, err = io.WriteString(part, "secret")
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost, "/upload/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	_, err = os.Stat(filepath.Join(cfg.Upload.Workspace, ".gitignore"))
	require.True(t, os.IsNotExist(err))
}

func TestUploadAddRejectsEmpty(t *testing.T) {
	h := NewServer(cfgWithWorkspace(t), &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, "/upload/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func postUploadFile(t *testing.T, h http.Handler, cookie *http.Cookie, name string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", name)
	require.NoError(t, err)
	_, err = io.WriteString(part, "data")
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, "/upload/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func putUploadState(t *testing.T, h http.Handler, cookie *http.Cookie, when, title string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"time": {when}, "title": {title}}
	req := httptest.NewRequest(http.MethodPut, "/upload/state", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func deleteUploadFile(t *testing.T, h http.Handler, cookie *http.Cookie, name string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/upload/files?name="+url.QueryEscape(name), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestUploadStateLifecycle(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)

	// No staged files: nothing pinned, PUT does not persist.
	require.Equal(t, workspaceState{}, loadWorkspaceState(dir))
	require.Equal(t, http.StatusOK, putUploadState(t, h, cookie, "2026-08-24T06:59", "draft").Code)
	require.Equal(t, workspaceState{}, loadWorkspaceState(dir))

	// First staged file pins the current time.
	postUploadFile(t, h, cookie, "a.txt")
	st := loadWorkspaceState(dir)
	require.NotEmpty(t, st.Time)
	_, err := time.Parse(pushTimeLayout, st.Time)
	require.NoError(t, err)

	// A second file does not move the pinned time.
	postUploadFile(t, h, cookie, "b.txt")
	require.Equal(t, st, loadWorkspaceState(dir))

	// Frontend edits are persisted.
	require.Equal(t, http.StatusOK, putUploadState(t, h, cookie, "2026-08-24T06:59", "weekly report").Code)
	require.Equal(t, workspaceState{Time: "2026-08-24T06:59", Title: "weekly report"}, loadWorkspaceState(dir))

	// Invalid time is rejected and keeps the old value.
	require.Equal(t, http.StatusBadRequest, putUploadState(t, h, cookie, "not-a-time", "x").Code)
	require.Equal(t, "2026-08-24T06:59", loadWorkspaceState(dir).Time)

	// The page prefills the pinned time and title.
	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `value="2026-08-24T06:59"`)
	require.Contains(t, rec.Body.String(), `value="weekly report"`)

	// Removing the last staged file clears the state.
	deleteUploadFile(t, h, cookie, "a.txt")
	require.Equal(t, "2026-08-24T06:59", loadWorkspaceState(dir).Time)
	deleteUploadFile(t, h, cookie, "b.txt")
	require.Equal(t, workspaceState{}, loadWorkspaceState(dir))
}
