package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// newFakeLLM serves scripted chat-completions responses; fn returns the raw
// JSON body for each call.
func newFakeLLM(t *testing.T, fn func(call int, r *http.Request, req chatRequest) string) *httptest.Server {
	t.Helper()
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		var req chatRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fn(call, r, req)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func postSuggest(t *testing.T, h http.Handler, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/upload/suggest", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestUploadSuggestRequiresLogin(t *testing.T) {
	h := NewServer(cfgWithWorkspace(t), &fakeStore{}).Handler()
	rec := postSuggest(t, h, nil)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))
}

func TestUploadSuggestNotConfigured(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("hi"), 0o644))
	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	require.Equal(t, http.StatusServiceUnavailable, postSuggest(t, h, cookie).Code)
}

func TestUploadSuggestNoFiles(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	cfg.LLM = LLMConfig{URL: "http://127.0.0.1:1/", Model: "m"}
	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	require.Equal(t, http.StatusBadRequest, postSuggest(t, h, cookie).Code)
}

func TestUploadSuggestSuccess(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world"), 0o644))
	require.NoError(t, saveWorkspaceState(dir, workspaceState{Time: "2026-08-24T06:59"}))

	srv := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
		switch call {
		case 1:
			require.Equal(t, "test-model", req.Model)
			require.Equal(t, "high", req.ReasoningEffort)
			require.Equal(t, "Bearer token", r.Header.Get("Authorization"))
			require.Len(t, req.Tools, 3)
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"read_text_file","arguments":"{\"name\":\"a.txt\"}"}}]}}]}`
		case 2:
			// The tool reply carries the file content back to the model.
			found := false
			for _, m := range req.Messages {
				if s, ok := m.Content.(string); ok && m.Role == "tool" && strings.Contains(s, "hello world") {
					found = true
				}
			}
			require.True(t, found)
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c2","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"weekly-report\"}"}}]}}]}`
		default:
			t.Fatalf("unexpected extra LLM call %d", call)
			return ""
		}
	})
	cfg.LLM = LLMConfig{URL: srv.URL, Model: "test-model", Effort: "high", Headers: map[string]string{"Authorization": "Bearer token"}}

	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	rec := postSuggest(t, h, cookie)
	require.Equal(t, http.StatusOK, rec.Code)
	var payload struct {
		Title string `json:"title"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, "weekly-report", payload.Title)

	// Title persisted, pinned time preserved.
	require.Equal(t, workspaceState{Time: "2026-08-24T06:59", Title: "weekly-report"}, loadWorkspaceState(dir))
}

func TestUploadSuggestImageTool(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	png := []byte{0x89, 'P', 'N', 'G'}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pic.png"), png, 0o644))

	srv := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
		switch call {
		case 1:
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"read_image_file","arguments":"{\"name\":\"pic.png\"}"}}]}}]}`
		case 2:
			// The image is appended as a user message with a base64 image_url part.
			found := false
			for _, m := range req.Messages {
				parts, ok := m.Content.([]any)
				if !ok || m.Role != "user" {
					continue
				}
				for _, p := range parts {
					if part, ok := p.(map[string]any); ok && part["type"] == "image_url" {
						if iu, ok := part["image_url"].(map[string]any); ok && strings.HasPrefix(iu["url"].(string), "data:image/png;base64,") {
							found = true
						}
					}
				}
			}
			require.True(t, found)
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c2","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"screenshot\"}"}}]}}]}`
		default:
			t.Fatalf("unexpected extra LLM call %d", call)
			return ""
		}
	})
	cfg.LLM = LLMConfig{URL: srv.URL, Model: "test-model"}

	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	rec := postSuggest(t, h, cookie)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "screenshot", loadWorkspaceState(dir).Title)
}

func TestUploadSuggestNoTitle(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("hi"), 0o644))
	srv := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
		return `{"choices":[{"message":{"role":"assistant","content":"weekly-report"}}]}`
	})
	cfg.LLM = LLMConfig{URL: srv.URL, Model: "test-model"}
	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	require.Equal(t, http.StatusBadGateway, postSuggest(t, h, cookie).Code)
}

func TestReadWorkspaceText(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{'a', 0, 'b'}, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644))

	text, err := readWorkspaceText(dir, "a.txt")
	require.NoError(t, err)
	require.Equal(t, "hello", text)

	_, err = readWorkspaceText(dir, "bin.dat")
	require.Error(t, err)

	text, err = readWorkspaceText(dir, "empty.txt")
	require.NoError(t, err)
	require.Equal(t, "(empty file)", text)

	_, err = readWorkspaceText(dir, "../secret")
	require.Error(t, err)

	_, err = readWorkspaceText(dir, ".hidden")
	require.Error(t, err)
}

func TestReadWorkspaceImage(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pic.PNG"), []byte("data"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644))

	mime, data, err := readWorkspaceImage(dir, "pic.PNG")
	require.NoError(t, err)
	require.Equal(t, "image/png", mime)
	require.Equal(t, []byte("data"), data)

	_, _, err = readWorkspaceImage(dir, "a.txt")
	require.Error(t, err)
}
