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
		{workspaceMetaDir, "", true},
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
	for _, path := range []string{"/upload", "/upload/files", "/upload/events"} {
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
	require.Contains(t, body, `<script defer src="`+jsAsset("upload")+`">`)
	require.NotContains(t, body, "id=\"analyze-btn\"")

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

func postUploadFileRec(t *testing.T, h http.Handler, cookie *http.Cookie, name string) *httptest.ResponseRecorder {
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
	return rec
}

func postUploadFile(t *testing.T, h http.Handler, cookie *http.Cookie, name string) {
	t.Helper()
	require.Equal(t, http.StatusOK, postUploadFileRec(t, h, cookie, name).Code)
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
	srv := NewServer(cfg, &fakeStore{})
	h := srv.Handler()
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

	// Editing time/title preserves the analyzed flag.
	require.NoError(t, srv.state.save(workspaceState{Time: "2026-08-24T06:59", Title: "weekly report", Analyzed: true}))
	require.Equal(t, http.StatusOK, putUploadState(t, h, cookie, "2026-08-24T06:59", "weekly report").Code)
	require.Equal(t, workspaceState{Time: "2026-08-24T06:59", Title: "weekly report", Analyzed: true}, loadWorkspaceState(dir))

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

func TestUploadPageShowsAnalyzeWhenLLMConfigured(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	cfg.LLM = LLMConfig{URL: "http://127.0.0.1:1/", Model: "m"}
	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `id="analyze-btn"`)
	require.Contains(t, rec.Body.String(), `<script defer src="`+jsAsset("upload")+`">`)
}

func TestPrepWorkspace(t *testing.T) {
	dir := t.TempDir()
	// Fresh and expired cache entries, plus an interrupted tmp write.
	require.NoError(t, os.MkdirAll(workspaceCachePath(dir), 0o755))
	fresh := filepath.Join(workspaceCachePath(dir), strings.Repeat("a", 64)+".txt")
	expired := filepath.Join(workspaceCachePath(dir), strings.Repeat("b", 64)+".txt")
	require.NoError(t, os.WriteFile(fresh, []byte("new"), 0o644))
	require.NoError(t, os.WriteFile(expired, []byte("old"), 0o644))
	old := time.Now().Add(-2 * convertCacheTTL)
	require.NoError(t, os.Chtimes(expired, old, old))
	require.NoError(t, os.MkdirAll(workspaceTmpPath(dir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceTmpPath(dir), "tmp-stale"), []byte("junk"), 0o644))

	require.NoError(t, prepWorkspace(dir))

	// Temp junk gone, staged files untouched.
	entries, err := os.ReadDir(workspaceTmpPath(dir))
	require.NoError(t, err)
	require.Empty(t, entries)
	// Cache pruned by TTL: fresh kept, expired removed.
	_, err = os.Stat(fresh)
	require.NoError(t, err)
	_, err = os.Stat(expired)
	require.True(t, os.IsNotExist(err))
}

func TestRenameWorkspaceFile(t *testing.T) {
	setup := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bbb"), 0o644))
		return dir
	}

	t.Run("ok", func(t *testing.T) {
		dir := setup(t)
		require.NoError(t, renameWorkspaceFile(dir, "a.txt", "renamed.txt"))
		data, err := os.ReadFile(filepath.Join(dir, "renamed.txt"))
		require.NoError(t, err)
		require.Equal(t, "aaa", string(data))
		_, err = os.Stat(filepath.Join(dir, "a.txt"))
		require.True(t, os.IsNotExist(err))
	})

	t.Run("same name", func(t *testing.T) {
		dir := setup(t)
		require.Error(t, renameWorkspaceFile(dir, "a.txt", "a.txt"))
	})

	t.Run("source missing", func(t *testing.T) {
		dir := setup(t)
		err := renameWorkspaceFile(dir, "missing.txt", "renamed.txt")
		require.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("target exists", func(t *testing.T) {
		dir := setup(t)
		require.Error(t, renameWorkspaceFile(dir, "a.txt", "b.txt"))
		// Neither file is touched.
		for name, want := range map[string]string{"a.txt": "aaa", "b.txt": "bbb"} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			require.NoError(t, err)
			require.Equal(t, want, string(data))
		}
	})

	t.Run("invalid new name", func(t *testing.T) {
		dir := setup(t)
		for _, bad := range []string{"", ".hidden", "a\x00b.txt"} {
			require.Error(t, renameWorkspaceFile(dir, "a.txt", bad), "%q", bad)
		}
	})
}
