package main

import (
	"context"
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
			require.Len(t, req.Tools, 4)
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"read_file_as_text","arguments":"{\"name\":\"a.txt\"}"}}]}}]}`
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
		Time  string `json:"time"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, "weekly-report", payload.Title)
	require.Empty(t, payload.Time)

	// Title persisted, pinned time preserved.
	require.Equal(t, workspaceState{Time: "2026-08-24T06:59", Title: "weekly-report"}, loadWorkspaceState(dir))
}

func TestUploadSuggestImageTool(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	png := append(append([]byte{}, pngSig...), []byte("rest")...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pic.png"), png, 0o644))

	srv := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
		switch call {
		case 1:
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"read_file_as_image","arguments":"{\"name\":\"pic.png\"}"}}]}}]}`
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
	rec := postSuggest(t, h, cookie)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "suggest failed\n", rec.Body.String())
}

func TestUnmarshalToolArgs(t *testing.T) {
	var args struct {
		Name  string `json:"name"`
		Title string `json:"title"`
	}
	require.NoError(t, unmarshalToolArgs(json.RawMessage(`{"title":"from-object"}`), &args))
	require.Equal(t, "from-object", args.Title)

	args = struct {
		Name  string `json:"name"`
		Title string `json:"title"`
	}{}
	require.NoError(t, unmarshalToolArgs(json.RawMessage(`"{\"name\":\"a.txt\"}"`), &args))
	require.Equal(t, "a.txt", args.Name)

	require.Error(t, unmarshalToolArgs(nil, &args))
	require.Error(t, unmarshalToolArgs(json.RawMessage(`null`), &args))
}

func TestUploadSuggestObjectArguments(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("hi"), 0o644))
	srv := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
		return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"set_title","arguments":{"title":"from-object"}}}]}}]}`
	})
	cfg.LLM = LLMConfig{URL: srv.URL, Model: "test-model"}
	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	rec := postSuggest(t, h, cookie)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "from-object", loadWorkspaceState(cfg.Upload.Workspace).Title)
}

func TestParseSuggestTime(t *testing.T) {
	got, err := parseSuggestTime(" 2026-08-20T15:04 ")
	require.NoError(t, err)
	require.Equal(t, "2026-08-20T15:04", got)

	got, err = parseSuggestTime("2026-08-20")
	require.NoError(t, err)
	require.Equal(t, "2026-08-20T00:00", got)

	_, err = parseSuggestTime("")
	require.Error(t, err)
	_, err = parseSuggestTime("not-a-time")
	require.Error(t, err)
}

func TestUploadSuggestSetDatetime(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("invoice 2026-08-20"), 0o644))
	require.NoError(t, saveWorkspaceState(dir, workspaceState{Time: "2026-08-24T06:59", Title: "old"}))

	srv := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
		switch call {
		case 1:
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"set_datetime","arguments":"{\"time\":\"2026-08-20\"}"}}]}}]}`
		case 2:
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c2","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"invoice\"}"}}]}}]}`
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
	var payload struct {
		Title string `json:"title"`
		Time  string `json:"time"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, "invoice", payload.Title)
	require.Equal(t, "2026-08-20T00:00", payload.Time)
	require.Equal(t, workspaceState{Time: "2026-08-20T00:00", Title: "invoice"}, loadWorkspaceState(dir))
}

func TestUploadSuggestInvalidDatetimeKeepsPinnedTime(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644))
	require.NoError(t, saveWorkspaceState(dir, workspaceState{Time: "2026-08-24T06:59"}))

	srv := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
		switch call {
		case 1:
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"set_datetime","arguments":"{\"time\":\"not-a-time\"}"}}]}}]}`
		case 2:
			found := false
			for _, m := range req.Messages {
				if s, ok := m.Content.(string); ok && m.Role == "tool" && strings.Contains(s, "invalid time") {
					found = true
				}
			}
			require.True(t, found)
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c2","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"ok\"}"}}]}}]}`
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
	require.Equal(t, workspaceState{Time: "2026-08-24T06:59", Title: "ok"}, loadWorkspaceState(dir))
}

func TestReadWorkspaceText(t *testing.T) {
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called for native text")
		return nil, nil
	})
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{'a', 0, 'b'}, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644))

	text, err := readWorkspaceText(context.Background(), dir, "a.txt")
	require.NoError(t, err)
	require.Equal(t, "hello", text)

	_, err = readWorkspaceText(context.Background(), dir, "bin.dat")
	require.Error(t, err)
	require.ErrorIs(t, err, errConvertUnavailable)

	text, err = readWorkspaceText(context.Background(), dir, "empty.txt")
	require.NoError(t, err)
	require.Equal(t, "(empty file)", text)

	_, err = readWorkspaceText(context.Background(), dir, "../secret")
	require.Error(t, err)

	_, err = readWorkspaceText(context.Background(), dir, ".hidden")
	require.Error(t, err)

	_, err = readWorkspaceText(context.Background(), dir, "pic.png")
	require.ErrorIs(t, err, errUseImageTool)
}

func TestReadWorkspaceImage(t *testing.T) {
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called for a small png")
		return nil, nil
	})
	dir := t.TempDir()
	png := append(append([]byte{}, pngSig...), []byte("rest")...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pic.PNG"), png, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644))

	mime, data, err := readWorkspaceImage(context.Background(), dir, "pic.PNG")
	require.NoError(t, err)
	require.Equal(t, "image/png", mime)
	require.Equal(t, png, data)

	_, _, err = readWorkspaceImage(context.Background(), dir, "a.txt")
	require.Error(t, err)
}
