package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSanitizePushTitle(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"weekly-report", "weekly-report", false},
		{"  weekly report  ", "weekly-report", false},
		{"报告 v1.2", "报告-v1.2", false},
		{"a/b\\c", "a-b-c", false},
		{"a   b", "a-b", false},
		{"..hidden..", "hidden", false},
		{"", "", true},
		{"   ", "", true},
		{"***", "", true},
		{"/...", "", true},
	}
	for _, tc := range cases {
		got, err := sanitizePushTitle(tc.in)
		if tc.wantErr {
			require.Error(t, err, tc.in)
			continue
		}
		require.NoError(t, err, tc.in)
		require.Equal(t, tc.want, got, tc.in)
	}
}

func TestSanitizePushTitleTruncates(t *testing.T) {
	in := make([]rune, 0, pushTitleMaxRunes+10)
	for i := 0; i < pushTitleMaxRunes+10; i++ {
		in = append(in, 'a')
	}
	got, err := sanitizePushTitle(string(in))
	require.NoError(t, err)
	require.Len(t, []rune(got), pushTitleMaxRunes)
}

func TestBundlePrefixPath(t *testing.T) {
	require.Equal(t, "content/55/0e/"+testBundleID1, bundlePrefix(testBundleID1))
}

func postPush(t *testing.T, h http.Handler, cookie *http.Cookie, when, title string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"time": {when}, "title": {title}}
	req := httptest.NewRequest(http.MethodPost, "/upload/push", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestUploadPushRequiresLogin(t *testing.T) {
	h := NewServer(cfgWithWorkspace(t), &fakeStore{}).Handler()
	req := httptest.NewRequest(http.MethodPost, "/upload/push", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))
}

func TestUploadPushValidation(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("a"), 0o644))
	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)

	require.Equal(t, http.StatusBadRequest, postPush(t, h, cookie, "not-a-time", "title").Code)
	require.Equal(t, http.StatusBadRequest, postPush(t, h, cookie, "2026-08-24T06:59", "***").Code)
	require.Equal(t, http.StatusBadRequest, postPush(t, h, cookie, "", "title").Code)
}

func TestUploadPushNoFiles(t *testing.T) {
	h := NewServer(cfgWithWorkspace(t), &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	require.Equal(t, http.StatusBadRequest, postPush(t, h, cookie, "2026-08-24T06:59", "title").Code)
}

func TestUploadPushSuccess(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("aaa"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "b.txt"), []byte("bb"), 0o644))
	require.NoError(t, saveWorkspaceState(cfg.Upload.Workspace, workspaceState{Time: "2026-08-24T06:59", Title: "weekly report"}))
	store := &fakeStore{}
	srv := NewServer(cfg, store)
	h := srv.Handler()
	cookie := loginCookie(t, h)

	rec := postPush(t, h, cookie, "2026-08-24T06:59", "weekly report")
	require.Equal(t, http.StatusAccepted, rec.Code)

	awaitIdle(t, srv)
	st := srv.lastJob()
	require.Empty(t, st.Error)
	require.True(t, strings.HasPrefix(st.Prefix, "content/"))
	require.True(t, strings.HasSuffix(st.Prefix, "/"))
	id, ok := parseBundleID(st.Prefix)
	require.True(t, ok)
	require.Equal(t, 2, st.Total)
	require.Equal(t, 2, st.Done)
	require.Equal(t, int64(5), st.TotalBytes)
	require.Equal(t, int64(5), st.DoneBytes)
	require.Equal(t, []string{
		bundlePrefix(id) + "/" + bundleMetaName,
		bundlePrefix(id) + "/a.txt",
		bundlePrefix(id) + "/b.txt",
		"index/2026/2026-08.json",
	}, store.putKeys())

	raw, err := store.Get(bundleMetaKey(id))
	require.NoError(t, err)
	var meta bundleMeta
	require.NoError(t, json.Unmarshal(raw, &meta))
	require.Equal(t, bundleMeta{ID: id, Title: "weekly-report", Time: "2026-08-24T06:59"}, meta)

	idx, err := store.Get("index/2026/2026-08.json")
	require.NoError(t, err)
	var listed []bundleMeta
	require.NoError(t, json.Unmarshal(idx, &listed))
	require.Equal(t, []bundleMeta{meta}, listed)

	// The in-memory index is updated too, so the calendar sees the bundle
	// without another bucket read.
	got, ok := srv.index.get(id)
	require.True(t, ok)
	require.Equal(t, meta, got)
	require.Equal(t, []bundleMeta{meta}, srv.index.year(2026))

	// Uploaded files are removed from the staging workspace (the .filestor
	// meta directory stays behind).
	files, err := listWorkspaceFiles(cfg.Upload.Workspace)
	require.NoError(t, err)
	require.Empty(t, files)
	// The pinned push state is cleared too.
	require.Equal(t, workspaceState{}, loadWorkspaceState(cfg.Upload.Workspace))
}

func TestUploadPushConflict(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("aaa"), 0o644))
	entered := make(chan struct{})
	var once sync.Once
	store := &fakeStore{
		putBlock: make(chan struct{}),
		putHook:  func(string) { once.Do(func() { close(entered) }) },
	}
	srv := NewServer(cfg, store)
	h := srv.Handler()
	cookie := loginCookie(t, h)

	require.Equal(t, http.StatusAccepted, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("push job did not start")
	}
	require.Equal(t, http.StatusConflict, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
	require.Equal(t, lockPush, srv.lockKind())

	close(store.putBlock)
	awaitIdle(t, srv)
	require.Empty(t, srv.lastJob().Error)
}

func TestWorkspaceLockDuringPush(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("aaa"), 0o644))
	entered := make(chan struct{})
	var once sync.Once
	store := &fakeStore{
		putBlock: make(chan struct{}),
		putHook:  func(string) { once.Do(func() { close(entered) }) },
	}
	srv := NewServer(cfg, store)
	h := srv.Handler()
	cookie := loginCookie(t, h)

	require.Equal(t, http.StatusAccepted, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("push job did not start")
	}

	require.Equal(t, http.StatusConflict, postUploadFileRec(t, h, cookie, "b.txt").Code)
	req := httptest.NewRequest(http.MethodDelete, "/upload/files?name=a.txt", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, http.StatusConflict, putUploadState(t, h, cookie, "2026-08-24T06:59", "x").Code)

	close(store.putBlock)
	awaitIdle(t, srv)
}

func TestUploadPushKeepsFilesAddedDuringJob(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0o644))
	require.NoError(t, saveWorkspaceState(dir, workspaceState{Time: "2026-08-24T06:59", Title: "t"}))
	entered := make(chan struct{})
	var once sync.Once
	store := &fakeStore{
		putBlock: make(chan struct{}),
		putHook: func(string) {
			once.Do(func() {
				_ = os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("x"), 0o644)
				close(entered)
			})
		},
	}
	srv := NewServer(cfg, store)
	h := srv.Handler()
	cookie := loginCookie(t, h)

	require.Equal(t, http.StatusAccepted, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("push job did not start")
	}
	close(store.putBlock)
	awaitIdle(t, srv)
	require.Empty(t, srv.lastJob().Error)

	_, err := os.Stat(filepath.Join(dir, "extra.txt"))
	require.NoError(t, err)
	require.Equal(t, workspaceState{Time: "2026-08-24T06:59", Title: "t"}, loadWorkspaceState(dir))
}

func TestUploadPushFailureKeepsFiles(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("aaa"), 0o644))
	store := &fakeStore{putErr: errors.New("oss down")}
	srv := NewServer(cfg, store)
	h := srv.Handler()
	cookie := loginCookie(t, h)

	require.Equal(t, http.StatusAccepted, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
	awaitIdle(t, srv)
	st := srv.lastJob()
	require.Contains(t, st.Error, bundleMetaName)
	require.Contains(t, st.Error, "oss down")

	// The failed file stays staged.
	_, err := os.Stat(filepath.Join(cfg.Upload.Workspace, "a.txt"))
	require.NoError(t, err)
	require.Empty(t, store.putKeys())

	// A later push can start over.
	store.putErr = nil
	require.Equal(t, http.StatusAccepted, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
	awaitIdle(t, srv)
	require.Empty(t, srv.lastJob().Error)
}

func TestUploadPushFileFailureSkipsIndex(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("aaa"), 0o644))
	store := &fakeStore{}
	store.putHook = func(key string) {
		if strings.HasSuffix(key, "/a.txt") {
			store.putErr = errors.New("oss down")
		}
	}
	srv := NewServer(cfg, store)
	h := srv.Handler()
	cookie := loginCookie(t, h)

	require.Equal(t, http.StatusAccepted, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
	awaitIdle(t, srv)
	st := srv.lastJob()
	require.Contains(t, st.Error, "a.txt")
	require.Contains(t, st.Error, "oss down")
	keys := store.putKeys()
	require.Len(t, keys, 1)
	require.True(t, strings.HasSuffix(keys[0], "/"+bundleMetaName))
	for _, k := range keys {
		require.False(t, strings.HasPrefix(k, "index/"), k)
	}
	// The in-memory index stays untouched too.
	require.Empty(t, srv.index.year(2026))

	_, err := os.Stat(filepath.Join(cfg.Upload.Workspace, "a.txt"))
	require.NoError(t, err)
}

func TestUploadPushRequiresAnalyze(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	// The gate only exists while the LLM is configured; the URL is never
	// called here, only its presence matters.
	cfg.LLM.Chat = ChatConfig{URL: "http://127.0.0.1:1/", Model: "m"}
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644))
	require.NoError(t, saveWorkspaceState(dir, workspaceState{Time: "2026-08-24T06:59", Title: "t"}))
	srv := NewServer(cfg, &fakeStore{})
	h := srv.Handler()
	cookie := loginCookie(t, h)

	// Not analyzed yet: push is rejected while the LLM is configured.
	require.Equal(t, http.StatusBadRequest, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)

	// A successful analyze run (flagged in the state) unlocks the push.
	require.NoError(t, srv.state.save(workspaceState{Time: "2026-08-24T06:59", Title: "t", Analyzed: true}))
	require.Equal(t, http.StatusAccepted, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
	awaitIdle(t, srv)
	require.Empty(t, srv.lastJob().Error)
}

func TestUploadAddClearsAnalyzed(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	cfg.LLM.Chat = ChatConfig{URL: "http://127.0.0.1:1/", Model: "m"}
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644))
	require.NoError(t, saveWorkspaceState(dir, workspaceState{Time: "2026-08-24T06:59", Title: "t", Analyzed: true}))
	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)

	require.Equal(t, http.StatusOK, postUploadFileRec(t, h, cookie, "b.txt").Code)
	// Only the analyzed flag is reset; the pinned time/title survive.
	require.Equal(t, workspaceState{Time: "2026-08-24T06:59", Title: "t"}, loadWorkspaceState(dir))
	require.Equal(t, http.StatusBadRequest, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
}

func TestUploadDeleteClearsAnalyzed(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	cfg.LLM.Chat = ChatConfig{URL: "http://127.0.0.1:1/", Model: "m"}
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644))
	require.NoError(t, saveWorkspaceState(dir, workspaceState{Time: "2026-08-24T06:59", Title: "t", Analyzed: true}))
	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodDelete, "/upload/files?name=a.txt", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	// The workspace is not empty, so the state file stays with the flag reset.
	require.Equal(t, workspaceState{Time: "2026-08-24T06:59", Title: "t"}, loadWorkspaceState(dir))
	require.Equal(t, http.StatusBadRequest, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
}

func TestUploadPushIndexFailureKeepsFiles(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bb"), 0o644))
	store := &fakeStore{}
	store.putHook = func(key string) {
		if strings.HasPrefix(key, indexRoot+"/") {
			store.putErr = errors.New("oss down")
		}
	}
	srv := NewServer(cfg, store)
	h := srv.Handler()
	cookie := loginCookie(t, h)

	require.Equal(t, http.StatusAccepted, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
	awaitIdle(t, srv)
	st := srv.lastJob()
	require.Contains(t, st.Error, "index:")
	require.Contains(t, st.Error, "oss down")
	// The failure carries the bundle prefix for log correlation.
	id, ok := parseBundleID(st.Prefix)
	require.True(t, ok)
	require.Contains(t, st.Error, "(bundle "+bundlePrefix(id)+")")

	// New semantics: the index write failed, so the bundle was never recorded
	// and every staged file stays in the workspace for a retry.
	for _, k := range store.putKeys() {
		require.False(t, strings.HasPrefix(k, indexRoot+"/"), k)
	}
	require.Empty(t, srv.index.year(2026))
	for _, name := range []string{"a.txt", "b.txt"} {
		_, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err, name)
	}
}
