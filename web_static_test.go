package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMatchAsset(t *testing.T) {
	files := []string{".gitkeep", "upload-f9kh7f96.js", "home.js", "upload2-abc12345.js"}
	require.Equal(t, "upload-f9kh7f96.js", matchAsset(files, "upload"))
	require.Equal(t, "home.js", matchAsset(files, "home"))
	require.Equal(t, "", matchAsset(files, "missing"))
	// The hash prefix match must not bleed into a longer entry name.
	require.Equal(t, "upload2-abc12345.js", matchAsset(files, "upload2"))
}

func TestJSAssetFallback(t *testing.T) {
	require.Equal(t, "/static/nope.js", jsAsset("nope"))
}

func TestStaticServesEmbeddedDist(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/static/.gitkeep", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
}

func TestStaticRequiresLogin(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/static/upload.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))
}
