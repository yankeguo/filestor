package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmbedContent(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		_, _ = w.Write([]byte(`{"output":{"embeddings":[{"index":0,"embedding":[0.1,0.2,0.3],"type":"text"}]}}`))
	}))
	t.Cleanup(srv.Close)

	ec := newEmbedClient(BailianMultimodalEmbeddingConfig{
		URL: srv.URL, Model: "vision-model", Dimensions: 1024,
		Headers: map[string]string{"Authorization": "Bearer sk-test"},
	})
	vec, err := ec.embedContent(context.Background(), map[string]string{"text": "hello"})
	require.NoError(t, err)
	require.Equal(t, []float32{0.1, 0.2, 0.3}, vec)
	require.Equal(t, "Bearer sk-test", gotAuth)
	require.Contains(t, gotBody, `"model":"vision-model"`)
	require.Contains(t, gotBody, `"text":"hello"`)
	require.Contains(t, gotBody, `"dimension":1024`)
}

func TestEmbedContentErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"http500", http.StatusInternalServerError, "upstream down", "HTTP 500"},
		{"apiCode", http.StatusOK, `{"code":"InvalidApiKey","message":"Invalid API-key provided."}`, "InvalidApiKey"},
		{"noVector", http.StatusOK, `{"output":{"embeddings":[]}}`, "no vector"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			ec := newEmbedClient(BailianMultimodalEmbeddingConfig{URL: srv.URL, Model: "m"})
			_, err := ec.embedContent(context.Background(), map[string]string{"text": "x"})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestEmbedDigest(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "text-01.txt"), []byte("a chunk"), 0o644))
	png := append(append([]byte{}, pngSig...), []byte("rest")...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "image-01-pic.png"), png, 0o644))

	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(data))
		_, _ = w.Write([]byte(`{"output":{"embeddings":[{"index":0,"embedding":[0.1,0.2],"type":"text"}]}}`))
	}))
	t.Cleanup(srv.Close)

	var progressed []string
	ec := newEmbedClient(BailianMultimodalEmbeddingConfig{URL: srv.URL, Model: "m"})
	vecs, err := ec.embedDigest(context.Background(), dir, []string{"text-01.txt", "image-01-pic.png"}, func(i int, name string) {
		progressed = append(progressed, name)
	})
	require.NoError(t, err)
	require.Len(t, vecs, 2)
	require.Equal(t, []string{"text-01.txt", "image-01-pic.png"}, progressed)

	require.Len(t, bodies, 2)
	require.Contains(t, bodies[0], `"text":"a chunk"`)
	require.Contains(t, bodies[1], `"image":"data:image/png;base64,`)
}

func TestEmbedDigestDimensionMismatch(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "text-01.txt"), []byte("one"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "text-02.txt"), []byte("two"), 0o644))

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"output":{"embeddings":[{"index":0,"embedding":[0.1,0.2],"type":"text"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"output":{"embeddings":[{"index":0,"embedding":[0.1,0.2,0.3],"type":"text"}]}}`))
	}))
	t.Cleanup(srv.Close)

	ec := newEmbedClient(BailianMultimodalEmbeddingConfig{URL: srv.URL, Model: "m"})
	_, err := ec.embedDigest(context.Background(), dir, []string{"text-01.txt", "text-02.txt"}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "dimension")
}
