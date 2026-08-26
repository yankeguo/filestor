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
	// .meta.json is published last, after every file landed.
	require.Equal(t, []string{
		bundlePrefix(id) + "/a.txt",
		bundlePrefix(id) + "/b.txt",
		bundlePrefix(id) + "/" + bundleMetaName,
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
	require.Contains(t, st.Error, "a.txt")
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
	// The first file already failed, so nothing was ever written — least of
	// all the index (and .meta.json is only published after every file).
	require.Empty(t, store.putKeys())
	// The in-memory index stays untouched too.
	require.Empty(t, srv.index.year(2026))

	_, err := os.Stat(filepath.Join(cfg.Upload.Workspace, "a.txt"))
	require.NoError(t, err)
}

func TestUploadPushRequiresAnalyze(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	// The gate only exists while the LLM is configured; the URL is never
	// called here, only its presence matters.
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: "http://127.0.0.1:1/", Model: "m"}
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
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: "http://127.0.0.1:1/", Model: "m"}
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
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: "http://127.0.0.1:1/", Model: "m"}
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

func TestUploadPushWithDigest(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0o644))
	require.NoError(t, saveWorkspaceState(dir, workspaceState{Time: "2026-08-24T06:59", Title: "t", Analyzed: true}))
	require.NoError(t, os.MkdirAll(workspaceDigestPath(dir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDigestPath(dir), "text-01.txt"), []byte("chunk"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDigestPath(dir), "image-01-pic.png"), []byte("png"), 0o644))
	store := &fakeStore{}
	srv := NewServer(cfg, store)
	h := srv.Handler()
	cookie := loginCookie(t, h)

	require.Equal(t, http.StatusAccepted, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
	awaitIdle(t, srv)
	st := srv.lastJob()
	require.Empty(t, st.Error)
	id, ok := parseBundleID(st.Prefix)
	require.True(t, ok)

	// The digest marks landed under the bundle's .digest directory (read in
	// alphabetical order) and count towards the file total; .meta.json is
	// published last.
	require.Equal(t, []string{
		bundlePrefix(id) + "/a.txt",
		bundlePrefix(id) + "/.digest/image-01-pic.png",
		bundlePrefix(id) + "/.digest/text-01.txt",
		bundlePrefix(id) + "/" + bundleMetaName,
		"index/2026/2026-08.json",
	}, store.putKeys())
	require.Equal(t, 3, st.Total)
	require.Equal(t, 3, st.Done)
	require.Equal(t, int64(3+3+5), st.TotalBytes)

	raw, err := store.Get(bundlePrefix(id) + "/.digest/text-01.txt")
	require.NoError(t, err)
	require.Equal(t, "chunk", string(raw))

	// The push emptied the staging area, so the state and the digest
	// directory are gone.
	require.Equal(t, workspaceState{}, loadWorkspaceState(dir))
	_, err = os.Stat(workspaceDigestPath(dir))
	require.True(t, os.IsNotExist(err))
}

func TestUploadPushDigestFailureSkipsIndex(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0o644))
	require.NoError(t, saveWorkspaceState(dir, workspaceState{Time: "2026-08-24T06:59", Title: "t", Analyzed: true}))
	require.NoError(t, os.MkdirAll(workspaceDigestPath(dir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDigestPath(dir), "text-01.txt"), []byte("chunk"), 0o644))
	store := &fakeStore{}
	store.putHook = func(key string) {
		if strings.Contains(key, "/.digest/") {
			store.putErr = errors.New("oss down")
		}
	}
	srv := NewServer(cfg, store)
	h := srv.Handler()
	cookie := loginCookie(t, h)

	require.Equal(t, http.StatusAccepted, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
	awaitIdle(t, srv)
	st := srv.lastJob()
	require.Contains(t, st.Error, ".digest/text-01.txt")
	require.Contains(t, st.Error, "oss down")
	// The index was never written and everything stays staged for a retry.
	for _, k := range store.putKeys() {
		require.False(t, strings.HasPrefix(k, indexRoot+"/"), k)
	}
	require.Empty(t, srv.index.year(2026))
	_, err := os.Stat(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(workspaceDigestPath(dir), "text-01.txt"))
	require.NoError(t, err)
}

func TestUploadPushNoStore(t *testing.T) {
	h := NewServer(cfgWithWorkspace(t), nil).Handler()
	cookie := loginCookie(t, h)
	require.Equal(t, http.StatusServiceUnavailable, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
}

// rewriteTransport redirects every request to target, keeping the original
// path and query: it points the OSS Vectors client at a test server while
// the config carries a real-shaped host.
type rewriteTransport struct{ target *url.URL }

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = t.target.Scheme
	req.URL.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestUploadPushEmbedsDigest(t *testing.T) {
	embedCalls := 0
	emb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		embedCalls++
		_, _ = w.Write([]byte(`{"output":{"embeddings":[{"index":0,"embedding":[0.1,0.2],"type":"text"}]}}`))
	}))
	t.Cleanup(emb.Close)
	var vecBody map[string]any
	vec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&vecBody)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(vec.Close)

	// Point the vectors client at the test server despite the configured host.
	target, err := url.Parse(vec.URL)
	require.NoError(t, err)
	oldClient := vectorsHTTPClient
	vectorsHTTPClient = &http.Client{Transport: rewriteTransport{target: target}}
	t.Cleanup(func() { vectorsHTTPClient = oldClient })

	cfg := cfgWithWorkspace(t)
	cfg.LLM.Embeddings.BailianMultimodalEmbedding = BailianMultimodalEmbeddingConfig{URL: emb.URL, Model: "m"}
	cfg.LLM.Vectors.AliyunOSSVectors = AliyunOSSVectorsConfig{
		URL:             "https://bkt.cn-hangzhou.oss-vectors.aliyuncs.com",
		AccessKeyID:     "ak",
		AccessKeySecret: "sk",
		Index:           "idx",
	}
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0o644))
	require.NoError(t, saveWorkspaceState(dir, workspaceState{Time: "2026-08-24T06:59", Title: "t", Analyzed: true}))
	require.NoError(t, os.MkdirAll(workspaceDigestPath(dir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDigestPath(dir), "text-01.txt"), []byte("chunk"), 0o644))
	png := append(append([]byte{}, pngSig...), []byte("rest")...)
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDigestPath(dir), "image-01-pic.png"), png, 0o644))
	store := &fakeStore{}
	srv := NewServer(cfg, store)
	h := srv.Handler()
	cookie := loginCookie(t, h)

	require.Equal(t, http.StatusAccepted, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
	awaitIdle(t, srv)
	require.Empty(t, srv.lastJob().Error)
	id, ok := parseBundleID(srv.lastJob().Prefix)
	require.True(t, ok)

	// One embedding request per digest file; one PutVectors carrying every
	// vector keyed by bundle id.
	require.Equal(t, 2, embedCalls)
	vs, ok := vecBody["vectors"].([]any)
	require.True(t, ok)
	require.Len(t, vs, 2)
	require.Equal(t, id+"/image-01-pic.png", vs[0].(map[string]any)["key"])
	require.Equal(t, id+"/text-01.txt", vs[1].(map[string]any)["key"])
	require.Equal(t, "idx", vecBody["indexName"])

	// The bundle completed and the workspace was cleaned.
	require.Equal(t, []string{
		bundlePrefix(id) + "/a.txt",
		bundlePrefix(id) + "/.digest/image-01-pic.png",
		bundlePrefix(id) + "/.digest/text-01.txt",
		bundlePrefix(id) + "/" + bundleMetaName,
		"index/2026/2026-08.json",
	}, store.putKeys())
	require.Equal(t, workspaceState{}, loadWorkspaceState(dir))
}

func TestUploadPushEmbedFailureSkipsIndex(t *testing.T) {
	emb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("model down"))
	}))
	t.Cleanup(emb.Close)

	cfg := cfgWithWorkspace(t)
	cfg.LLM.Embeddings.BailianMultimodalEmbedding = BailianMultimodalEmbeddingConfig{URL: emb.URL, Model: "m"}
	cfg.LLM.Vectors.AliyunOSSVectors = AliyunOSSVectorsConfig{
		URL:             "https://bkt.cn-hangzhou.oss-vectors.aliyuncs.com",
		AccessKeyID:     "ak",
		AccessKeySecret: "sk",
		Index:           "idx",
	}
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0o644))
	require.NoError(t, saveWorkspaceState(dir, workspaceState{Time: "2026-08-24T06:59", Title: "t", Analyzed: true}))
	require.NoError(t, os.MkdirAll(workspaceDigestPath(dir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDigestPath(dir), "text-01.txt"), []byte("chunk"), 0o644))
	store := &fakeStore{}
	srv := NewServer(cfg, store)
	h := srv.Handler()
	cookie := loginCookie(t, h)

	require.Equal(t, http.StatusAccepted, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
	awaitIdle(t, srv)
	st := srv.lastJob()
	require.Contains(t, st.Error, "embed")
	require.Contains(t, st.Error, "model down")

	// The embedding failed before meta and index: the bundle is neither
	// discoverable nor recorded, and everything stays staged for a retry.
	for _, k := range store.putKeys() {
		require.False(t, strings.HasSuffix(k, bundleMetaName), k)
		require.False(t, strings.HasPrefix(k, indexRoot+"/"), k)
	}
	require.Empty(t, srv.index.year(2026))
	_, err := os.Stat(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(workspaceDigestPath(dir), "text-01.txt"))
	require.NoError(t, err)
}
