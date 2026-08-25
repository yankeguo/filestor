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
	"time"

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

func postAnalyze(t *testing.T, h http.Handler, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/upload/analyze", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestUploadAnalyzeRequiresLogin(t *testing.T) {
	h := NewServer(cfgWithWorkspace(t), &fakeStore{}).Handler()
	rec := postAnalyze(t, h, nil)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))
}

func TestUploadAnalyzeNotConfigured(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("hi"), 0o644))
	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	require.Equal(t, http.StatusServiceUnavailable, postAnalyze(t, h, cookie).Code)
}

func TestUploadAnalyzeNoFiles(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	cfg.LLM = LLMConfig{URL: "http://127.0.0.1:1/", Model: "m"}
	h := NewServer(cfg, &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	require.Equal(t, http.StatusBadRequest, postAnalyze(t, h, cookie).Code)
}

func TestUploadAnalyzeSuccess(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world"), 0o644))
	require.NoError(t, saveWorkspaceState(dir, workspaceState{Time: "2026-08-24T06:59"}))

	llm := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
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
	cfg.LLM = LLMConfig{URL: llm.URL, Model: "test-model", Effort: "high", Headers: map[string]string{"Authorization": "Bearer token"}}

	app := NewServer(cfg, &fakeStore{})
	h := app.Handler()
	cookie := loginCookie(t, h)
	rec := postAnalyze(t, h, cookie)
	require.Equal(t, http.StatusAccepted, rec.Code)
	awaitIdle(t, app)
	require.Equal(t, workspaceState{Time: "2026-08-24T06:59", Title: "weekly-report", Analyzed: true}, loadWorkspaceState(dir))
	require.Equal(t, "weekly-report", app.lastJob().Title)
	require.Empty(t, app.lastJob().Error)
}

func TestUploadAnalyzeImageTool(t *testing.T) {
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

	app := NewServer(cfg, &fakeStore{})
	h := app.Handler()
	cookie := loginCookie(t, h)
	rec := postAnalyze(t, h, cookie)
	require.Equal(t, http.StatusAccepted, rec.Code)
	awaitIdle(t, app)
	require.Equal(t, "screenshot", loadWorkspaceState(dir).Title)
}

func TestUploadAnalyzeTextAnswerFallback(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644))
	srv := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
		if call == 2 {
			// The forced-decision round only offers set_title/set_datetime.
			require.Len(t, req.Tools, 2)
		}
		// The model never calls set_title and answers in plain text.
		return `{"choices":[{"message":{"role":"assistant","content":"weekly-report"}}]}`
	})
	cfg.LLM = LLMConfig{URL: srv.URL, Model: "test-model"}
	app := NewServer(cfg, &fakeStore{})
	h := app.Handler()
	cookie := loginCookie(t, h)
	rec := postAnalyze(t, h, cookie)
	require.Equal(t, http.StatusAccepted, rec.Code)
	awaitIdle(t, app)
	require.Equal(t, "weekly-report", app.lastJob().Title)
	require.Empty(t, app.lastJob().Error)
	require.Equal(t, "weekly-report", loadWorkspaceState(dir).Title)
}

func TestUploadAnalyzeNoTitle(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("hi"), 0o644))
	srv := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
		// Neither a tool call nor usable text: genuinely no title.
		return `{"choices":[{"message":{"role":"assistant","content":""}}]}`
	})
	cfg.LLM = LLMConfig{URL: srv.URL, Model: "test-model"}
	app := NewServer(cfg, &fakeStore{})
	h := app.Handler()
	cookie := loginCookie(t, h)
	rec := postAnalyze(t, h, cookie)
	require.Equal(t, http.StatusAccepted, rec.Code)
	awaitIdle(t, app)
	require.Equal(t, "analyze failed", app.lastJob().Error)
	require.Empty(t, loadWorkspaceState(cfg.Upload.Workspace).Title)
}

func TestUploadAnalyzeRoundBudgetForcesDecision(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644))
	calls := 0
	srv := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
		calls = call
		if call <= analyzeMaxRounds {
			// The model keeps reading the same file, burning every round.
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c","type":"function","function":{"name":"read_file_as_text","arguments":"{\"name\":\"a.txt\"}"}}]}}]}`
		}
		require.Equal(t, analyzeMaxRounds+1, call)
		require.Len(t, req.Tools, 2, "forced round drops the read tools")
		nudged := false
		for _, m := range req.Messages {
			if s, ok := m.Content.(string); ok && m.Role == "user" && strings.Contains(s, "decide now") {
				nudged = true
			}
		}
		require.True(t, nudged)
		return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
			`{"id":"c2","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"forced\"}"}}]}}]}`
	})
	cfg.LLM = LLMConfig{URL: srv.URL, Model: "test-model"}
	app := NewServer(cfg, &fakeStore{})
	h := app.Handler()
	cookie := loginCookie(t, h)
	rec := postAnalyze(t, h, cookie)
	require.Equal(t, http.StatusAccepted, rec.Code)
	awaitIdle(t, app)
	require.Equal(t, "forced", loadWorkspaceState(dir).Title)
	require.Equal(t, analyzeMaxRounds+1, calls)
}

func TestUploadAnalyzePeeks(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "report.txt"), []byte("Q3 revenue\nsummary\nand outlook"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{'x', 0, 'y'}, 0o644))
	srv := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
		require.Equal(t, 1, call)
		list := ""
		for _, m := range req.Messages {
			if s, ok := m.Content.(string); ok && m.Role == "user" && strings.Contains(s, "Files staged for upload") {
				list = s
			}
		}
		// Native text gets a one-line peek; the binary file gets none.
		require.Contains(t, list, "peek: Q3 revenue summary and outlook")
		require.Equal(t, 1, strings.Count(list, "peek:"))
		return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"q3-report\"}"}}]}}]}`
	})
	cfg.LLM = LLMConfig{URL: srv.URL, Model: "test-model"}
	app := NewServer(cfg, &fakeStore{})
	h := app.Handler()
	cookie := loginCookie(t, h)
	rec := postAnalyze(t, h, cookie)
	require.Equal(t, http.StatusAccepted, rec.Code)
	awaitIdle(t, app)
	require.Equal(t, "q3-report", loadWorkspaceState(dir).Title)
}

func TestTextPeek(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n world\t!"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{'a', 0}, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644))

	require.Equal(t, "hello world !", textPeek(dir, "a.txt"))
	require.Empty(t, textPeek(dir, "bin.dat"))
	require.Empty(t, textPeek(dir, "empty.txt"))
	require.Empty(t, textPeek(dir, "missing.txt"))
	require.Empty(t, textPeek(dir, "../secret"))

	long := strings.Repeat("x", analyzePeekMaxBytes+100)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "long.txt"), []byte(long), 0o644))
	require.Len(t, textPeek(dir, "long.txt"), analyzePeekMaxBytes)
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

func TestUploadAnalyzeObjectArguments(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("hi"), 0o644))
	srv := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
		return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"set_title","arguments":{"title":"from-object"}}}]}}]}`
	})
	cfg.LLM = LLMConfig{URL: srv.URL, Model: "test-model"}
	app := NewServer(cfg, &fakeStore{})
	h := app.Handler()
	cookie := loginCookie(t, h)
	rec := postAnalyze(t, h, cookie)
	require.Equal(t, http.StatusAccepted, rec.Code)
	awaitIdle(t, app)
	require.Equal(t, "from-object", loadWorkspaceState(cfg.Upload.Workspace).Title)
}

func TestParseAnalyzeTime(t *testing.T) {
	got, err := parseAnalyzeTime(" 2026-08-20T15:04 ")
	require.NoError(t, err)
	require.Equal(t, "2026-08-20T15:04", got)

	got, err = parseAnalyzeTime("2026-08-20")
	require.NoError(t, err)
	require.Equal(t, "2026-08-20T00:00", got)

	_, err = parseAnalyzeTime("")
	require.Error(t, err)
	_, err = parseAnalyzeTime("not-a-time")
	require.Error(t, err)
}

func TestUploadAnalyzeSetDatetime(t *testing.T) {
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
	app := NewServer(cfg, &fakeStore{})
	h := app.Handler()
	cookie := loginCookie(t, h)
	rec := postAnalyze(t, h, cookie)
	require.Equal(t, http.StatusAccepted, rec.Code)
	awaitIdle(t, app)
	require.Equal(t, "invoice", app.lastJob().Title)
	require.Equal(t, "2026-08-20T00:00", app.lastJob().Time)
	require.Equal(t, workspaceState{Time: "2026-08-20T00:00", Title: "invoice", Analyzed: true}, loadWorkspaceState(dir))
}

func TestUploadAnalyzeInvalidDatetimeKeepsPinnedTime(t *testing.T) {
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
	app := NewServer(cfg, &fakeStore{})
	h := app.Handler()
	cookie := loginCookie(t, h)
	rec := postAnalyze(t, h, cookie)
	require.Equal(t, http.StatusAccepted, rec.Code)
	awaitIdle(t, app)
	require.Equal(t, workspaceState{Time: "2026-08-24T06:59", Title: "ok", Analyzed: true}, loadWorkspaceState(dir))
}

func TestUploadAnalyzeFailureKeepsUnanalyzed(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644))
	require.NoError(t, saveWorkspaceState(dir, workspaceState{Time: "2026-08-24T06:59"}))
	srv := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
		// Neither a tool call nor usable text: genuinely no title.
		return `{"choices":[{"message":{"role":"assistant","content":""}}]}`
	})
	cfg.LLM = LLMConfig{URL: srv.URL, Model: "test-model"}
	app := NewServer(cfg, &fakeStore{})
	h := app.Handler()
	cookie := loginCookie(t, h)
	require.Equal(t, http.StatusAccepted, postAnalyze(t, h, cookie).Code)
	awaitIdle(t, app)
	require.Equal(t, "analyze failed", app.lastJob().Error)
	require.False(t, loadWorkspaceState(dir).Analyzed)
}

func TestWorkspaceLockDuringAnalyze(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("hi"), 0o644))
	block := make(chan struct{})
	llm := newFakeLLM(t, func(call int, r *http.Request, req chatRequest) string {
		<-block
		return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"ok\"}"}}]}}]}`
	})
	cfg.LLM = LLMConfig{URL: llm.URL, Model: "test-model"}
	app := NewServer(cfg, &fakeStore{})
	h := app.Handler()
	cookie := loginCookie(t, h)
	require.Equal(t, http.StatusAccepted, postAnalyze(t, h, cookie).Code)
	require.Eventually(t, func() bool { return app.lockKind() == lockAnalyze }, 2*time.Second, 10*time.Millisecond)

	require.Equal(t, http.StatusConflict, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
	require.Equal(t, http.StatusConflict, postUploadFileRec(t, h, cookie, "b.txt").Code)
	require.Equal(t, http.StatusConflict, putUploadState(t, h, cookie, "2026-08-24T06:59", "x").Code)
	require.Equal(t, http.StatusConflict, postAnalyze(t, h, cookie).Code)

	close(block)
	awaitIdle(t, app)
	require.Equal(t, "ok", loadWorkspaceState(cfg.Upload.Workspace).Title)
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
