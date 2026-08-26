package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var errLLMAssert = errors.New("fake llm assertion failed")

// llmAssert collects assertion failures from the fake-LLM handler goroutine:
// testify's FailNow is only legal on the test goroutine, so fn receives a
// *require.Assertions bound to this recorder and newFakeLLM reports the
// collected failures from a cleanup on the test goroutine.
type llmAssert struct {
	mu       sync.Mutex
	failures []string
}

func (a *llmAssert) Errorf(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures = append(a.failures, fmt.Sprintf(format, args...))
}

// FailNow panics instead of Goexit; the handler recovers the panic, so a
// failed require stops fn without taking down the handler goroutine.
func (a *llmAssert) FailNow() { panic(errLLMAssert) }

func (a *llmAssert) check(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range a.failures {
		t.Error(f)
	}
}

// newFakeLLM serves scripted chat-completions responses; fn returns the raw
// JSON body for each call. Assertions inside fn must use rq, not t (fn runs on
// a handler goroutine, where require's FailNow is not allowed).
func newFakeLLM(t *testing.T, fn func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string) *httptest.Server {
	t.Helper()
	assertions := &llmAssert{}
	t.Cleanup(func() { assertions.check(t) })
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil && p != errLLMAssert {
				panic(p)
			}
		}()
		call++
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			assertions.Errorf("call %d: decode request: %v", call, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fn(require.New(assertions), call, r, req)))
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

	llm := newFakeLLM(t, func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string {
		switch call {
		case 1:
			rq.Equal("test-model", req.Model)
			rq.Equal("high", req.ReasoningEffort)
			rq.Equal("Bearer token", r.Header.Get("Authorization"))
			rq.Len(req.Tools, 5)
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
			rq.True(found)
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c2","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"weekly-report\"}"}}]}}]}`
		default:
			rq.Failf("unexpected extra LLM call", "call %d", call)
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

func TestUploadAnalyzeRename(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "IMG_2048.txt"), []byte("invoice for march"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "march-invoice.txt"), []byte("other"), 0o644))

	llm := newFakeLLM(t, func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string {
		switch call {
		case 1:
			// The target name is taken: the rename must fail without touching
			// either file.
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"rename_file","arguments":"{\"name\":\"IMG_2048.txt\",\"new_name\":\"march-invoice.txt\"}"}}]}}]}`
		case 2:
			found := false
			for _, m := range req.Messages {
				if s, ok := m.Content.(string); ok && m.Role == "tool" && strings.Contains(s, "error:") {
					found = true
				}
			}
			rq.True(found, "rename conflict must surface as a tool error")
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c2","type":"function","function":{"name":"rename_file","arguments":"{\"name\":\"IMG_2048.txt\",\"new_name\":\"2026-03-invoice.txt\"}"}}]}}]}`
		case 3:
			// The tool reply echoes the new name so later reads can use it.
			found := false
			for _, m := range req.Messages {
				if s, ok := m.Content.(string); ok && m.Role == "tool" && strings.Contains(s, "renamed to `2026-03-invoice.txt`") {
					found = true
				}
			}
			rq.True(found)
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c3","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"march-invoice\"}"}}]}}]}`
		default:
			rq.Failf("unexpected extra LLM call", "call %d", call)
			return ""
		}
	})
	cfg.LLM = LLMConfig{URL: llm.URL, Model: "test-model"}

	app := NewServer(cfg, &fakeStore{})
	h := app.Handler()
	cookie := loginCookie(t, h)
	require.Equal(t, http.StatusAccepted, postAnalyze(t, h, cookie).Code)
	awaitIdle(t, app)
	require.Equal(t, "march-invoice", loadWorkspaceState(dir).Title)

	// The rename landed on disk; the conflicting file kept its content.
	data, err := os.ReadFile(filepath.Join(dir, "2026-03-invoice.txt"))
	require.NoError(t, err)
	require.Equal(t, "invoice for march", string(data))
	_, err = os.Stat(filepath.Join(dir, "IMG_2048.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
	data, err = os.ReadFile(filepath.Join(dir, "march-invoice.txt"))
	require.NoError(t, err)
	require.Equal(t, "other", string(data))
}

func TestUploadAnalyzeImageTool(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	png := append(append([]byte{}, pngSig...), []byte("rest")...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pic.png"), png, 0o644))

	srv := newFakeLLM(t, func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string {
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
			rq.True(found)
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c2","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"screenshot\"}"}}]}}]}`
		default:
			rq.Failf("unexpected extra LLM call", "call %d", call)
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

func TestUploadAnalyzeMultiImageRepliesStayConsecutive(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	png := append(append([]byte{}, pngSig...), []byte("rest")...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.png"), png, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.png"), png, 0o644))

	srv := newFakeLLM(t, func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string {
		switch call {
		case 1:
			// Two image reads in a single assistant turn.
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"read_file_as_image","arguments":"{\"name\":\"a.png\"}"}},` +
				`{"id":"c2","type":"function","function":{"name":"read_file_as_image","arguments":"{\"name\":\"b.png\"}"}}]}}]}`
		case 2:
			// The assistant turn must be followed by consecutive tool replies;
			// the user messages carrying the images come after them, or strict
			// providers reject the request ("insufficient tool messages").
			idx := -1
			for i, m := range req.Messages {
				if m.Role == "assistant" && len(m.ToolCalls) == 2 {
					idx = i
				}
			}
			rq.GreaterOrEqual(idx, 0)
			rq.GreaterOrEqual(len(req.Messages), idx+5)
			ms := req.Messages[idx+1 : idx+5]
			rq.Equal("tool", ms[0].Role)
			rq.Equal("c1", ms[0].ToolCallID)
			rq.Equal("tool", ms[1].Role)
			rq.Equal("c2", ms[1].ToolCallID)
			rq.Equal("user", ms[2].Role)
			rq.Equal("user", ms[3].Role)
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c3","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"photos\"}"}}]}}]}`
		default:
			rq.Failf("unexpected extra LLM call", "call %d", call)
			return ""
		}
	})
	cfg.LLM = LLMConfig{URL: srv.URL, Model: "test-model"}
	app := NewServer(cfg, &fakeStore{})
	h := app.Handler()
	cookie := loginCookie(t, h)
	require.Equal(t, http.StatusAccepted, postAnalyze(t, h, cookie).Code)
	awaitIdle(t, app)
	require.Equal(t, "photos", loadWorkspaceState(dir).Title)
}

func TestUploadAnalyzeTextAnswerFallback(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644))
	srv := newFakeLLM(t, func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string {
		if call == 2 {
			// The forced-decision round only offers set_title/set_datetime.
			rq.Len(req.Tools, 2)
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
	srv := newFakeLLM(t, func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string {
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
	srv := newFakeLLM(t, func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string {
		calls = call
		if call <= analyzeBaseRounds {
			// The model keeps reading the same file, burning every round.
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c","type":"function","function":{"name":"read_file_as_text","arguments":"{\"name\":\"a.txt\"}"}}]}}]}`
		}
		rq.Equal(analyzeBaseRounds+1, call)
		rq.Len(req.Tools, 2, "forced round drops the read tools")
		nudged := false
		for _, m := range req.Messages {
			if s, ok := m.Content.(string); ok && m.Role == "user" && strings.Contains(s, "decide now") {
				nudged = true
			}
		}
		rq.True(nudged)
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
	require.Equal(t, analyzeBaseRounds+1, calls)
}

func TestAnalyzeBudget(t *testing.T) {
	cases := []struct {
		files         int
		rounds, calls int
	}{
		{0, analyzeBaseRounds, analyzeBaseToolCalls},
		{1, analyzeBaseRounds, analyzeBaseToolCalls},
		{2, analyzeBaseRounds + 1, analyzeBaseToolCalls + 2},
		{5, analyzeBaseRounds + 4, analyzeBaseToolCalls + 8},
		{100, analyzeRoundsCap, analyzeBaseToolCalls + 2*99},
		{10000, analyzeRoundsCap, analyzeToolCallsCap},
	}
	for _, tc := range cases {
		rounds, calls := analyzeBudget(tc.files)
		require.Equal(t, tc.rounds, rounds, "files=%d", tc.files)
		require.Equal(t, tc.calls, calls, "files=%d", tc.files)
	}
}

func TestUploadAnalyzePeeks(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "report.txt"), []byte("Q3 revenue\nsummary\nand outlook"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{'x', 0, 'y'}, 0o644))
	srv := newFakeLLM(t, func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string {
		rq.Equal(1, call)
		list := ""
		for _, m := range req.Messages {
			if s, ok := m.Content.(string); ok && m.Role == "user" && strings.Contains(s, "Files staged for upload") {
				list = s
			}
		}
		// Native text gets a one-line peek; the binary file gets none.
		rq.Contains(list, "peek: Q3 revenue summary and outlook")
		rq.Equal(1, strings.Count(list, "peek:"))
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
	srv := newFakeLLM(t, func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string {
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

	srv := newFakeLLM(t, func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string {
		switch call {
		case 1:
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"set_datetime","arguments":"{\"time\":\"2026-08-20\"}"}}]}}]}`
		case 2:
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c2","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"invoice\"}"}}]}}]}`
		default:
			rq.Failf("unexpected extra LLM call", "call %d", call)
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

	srv := newFakeLLM(t, func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string {
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
			rq.True(found)
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c2","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"ok\"}"}}]}}]}`
		default:
			rq.Failf("unexpected extra LLM call", "call %d", call)
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
	srv := newFakeLLM(t, func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string {
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
	llm := newFakeLLM(t, func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string {
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

func TestUploadAnalyzeUpstreamFailureNotEchoed(t *testing.T) {
	// Upstream failure details stay in the server logs: the job/SSE error is a
	// plain "analyze failed" and never carries the upstream response body.
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"http500", http.StatusInternalServerError, "secret-upstream-detail"},
		{"badJSON", http.StatusOK, "secret-upstream-detail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cfgWithWorkspace(t)
			require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("hi"), 0o644))
			llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(llm.Close)
			cfg.LLM = LLMConfig{URL: llm.URL, Model: "m"}
			app := NewServer(cfg, &fakeStore{})
			h := app.Handler()
			cookie := loginCookie(t, h)
			events := app.hub.subscribe()
			defer app.hub.unsubscribe(events)

			require.Equal(t, http.StatusAccepted, postAnalyze(t, h, cookie).Code)
			awaitIdle(t, app)
			require.Equal(t, "analyze failed", app.lastJob().Error)

			// No SSE event carries the upstream body either.
		drain:
			for {
				select {
				case ev, ok := <-events:
					if !ok {
						break drain
					}
					data, err := json.Marshal(ev.Data)
					require.NoError(t, err)
					require.NotContains(t, string(data), "secret-upstream-detail")
				default:
					break drain
				}
			}
		})
	}
}
