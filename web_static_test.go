package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMatchAsset(t *testing.T) {
	files := []string{".gitkeep", "upload-f9kh7f96.js", "home.js", "upload2-abc12345.js", "main-03dps6ge.css"}
	require.Equal(t, "upload-f9kh7f96.js", matchAsset(files, "upload", "js"))
	require.Equal(t, "home.js", matchAsset(files, "home", "js"))
	require.Equal(t, "", matchAsset(files, "missing", "js"))
	// The hash prefix match must not bleed into a longer entry name.
	require.Equal(t, "upload2-abc12345.js", matchAsset(files, "upload2", "js"))
	// A hashed stylesheet matches only for the css extension.
	require.Equal(t, "main-03dps6ge.css", matchAsset(files, "main", "css"))
	require.Equal(t, "", matchAsset(files, "main", "js"))
}

func TestJSAssetFallback(t *testing.T) {
	require.Equal(t, "/static/nope.js", jsAsset("nope"))
	require.Equal(t, "/static/nope.css", cssAsset("nope"))
}

func TestStaticServesEmbeddedDist(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()

	// /static/ is public: the login page itself links the stylesheet, and the
	// hashed UI bundles carry no secrets.
	req := httptest.NewRequest(http.MethodGet, "/static/.gitkeep", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
}
