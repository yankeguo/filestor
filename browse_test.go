package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeStore struct {
	page     ListPage
	signed   string
	err      error
	lastList [2]string
	lastKey  string
}

func (f *fakeStore) List(prefix, marker string) (ListPage, error) {
	f.lastList = [2]string{prefix, marker}
	return f.page, f.err
}

func (f *fakeStore) SignGetURL(key string, ttl time.Duration) (string, error) {
	f.lastKey = key
	if f.err != nil {
		return "", f.err
	}
	if f.signed != "" {
		return f.signed, nil
	}
	return "https://example.oss-cn-hangzhou.aliyuncs.com/" + url.PathEscape(key) + "?sig=1", nil
}

func TestBrowseListsDirsAndFiles(t *testing.T) {
	store := &fakeStore{page: ListPage{
		Prefixes: []string{"docs/"},
		Objects: []ObjectInfo{
			{Key: "readme.txt", Size: 12, LastModified: time.Date(2026, 8, 24, 4, 0, 0, 0, time.UTC)},
		},
	}}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "docs")
	require.Contains(t, body, `href="/?prefix=docs%2f"`)
	require.Contains(t, body, "readme.txt")
	require.Contains(t, body, `/download?key=readme.txt`)
	require.Equal(t, "", store.lastList[0])
}

func TestBrowsePrefixAndParent(t *testing.T) {
	store := &fakeStore{page: ListPage{
		Objects: []ObjectInfo{{Key: "docs/a.txt", Size: 1}},
	}}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/?prefix=docs", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "docs/", store.lastList[0])
	require.Contains(t, rec.Body.String(), `href="/"`)
	require.Contains(t, rec.Body.String(), "a.txt")
	require.Contains(t, rec.Body.String(), `/download?key=docs%2fa.txt`)
}

func TestBrowseNextPage(t *testing.T) {
	store := &fakeStore{page: ListPage{
		Objects:     []ObjectInfo{{Key: "a.txt", Size: 1}},
		IsTruncated: true,
		NextMarker:  "a.txt",
	}}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Contains(t, rec.Body.String(), "marker=a.txt")
}

func TestDownloadRedirectsToSignedURL(t *testing.T) {
	store := &fakeStore{signed: "https://oss.example/signed-object"}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/download?key=docs/a.txt", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "https://oss.example/signed-object", rec.Header().Get("Location"))
	require.Equal(t, "docs/a.txt", store.lastKey)
}

func TestDownloadRejectsEmptyKey(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	req := httptest.NewRequest(http.MethodGet, "/download", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDownloadRequiresLogin(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/download?key=a.txt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))
}

func TestBrowseListError(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{err: fmt.Errorf("boom")}).Handler()
	cookie := loginCookie(t, h)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestNormalizePrefixAndParent(t *testing.T) {
	require.Equal(t, "", normalizePrefix(""))
	require.Equal(t, "docs/", normalizePrefix("docs"))
	require.Equal(t, "docs/a/", normalizePrefix("/docs/a/"))

	p, ok := parentPrefix("")
	require.False(t, ok)
	require.Equal(t, "", p)

	p, ok = parentPrefix("docs/")
	require.True(t, ok)
	require.Equal(t, "", p)

	p, ok = parentPrefix("docs/a/")
	require.True(t, ok)
	require.Equal(t, "docs/", p)
}

func TestFormatSize(t *testing.T) {
	require.Equal(t, "12 B", formatSize(12))
	require.Equal(t, "1.0 KB", formatSize(1024))
	require.Equal(t, "1.5 KB", formatSize(1536))
}

func TestAttachmentDisposition(t *testing.T) {
	d := attachmentDisposition(`dir/"q".txt`)
	require.Contains(t, d, `filename="q.txt"`)
	require.Contains(t, d, "filename*=UTF-8''")
}
