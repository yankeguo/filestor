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
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: "http://127.0.0.1:1/", Model: "m"}
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
			rq.Len(req.Tools, 8)
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"name\":\"a.txt\"}"}}]}}]}`
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
				`{"id":"c2","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"weekly-report\"}"}},` +
				`{"id":"c3","type":"function","function":{"name":"finish","arguments":"{}"}}]}}]}`
		default:
			rq.Failf("unexpected extra LLM call", "call %d", call)
			return ""
		}
	})
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: llm.URL, Model: "test-model", Effort: "high", Headers: map[string]string{"Authorization": "Bearer token"}}

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
			// The tool reply echoes the new name so later reads can use it,
			// and carries the complete updated roster (the other staged file
			// shows up under its own name).
			found := false
			roster := false
			for _, m := range req.Messages {
				if s, ok := m.Content.(string); ok && m.Role == "tool" {
					if strings.Contains(s, "renamed to `2026-03-invoice.txt`") {
						found = true
						roster = strings.Contains(s, "march-invoice.txt")
					}
				}
			}
			rq.True(found)
			rq.True(roster, "rename reply must carry the complete updated roster")
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c3","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"march-invoice\"}"}},` +
				`{"id":"c4","type":"function","function":{"name":"finish","arguments":"{}"}}]}}]}`
		default:
			rq.Failf("unexpected extra LLM call", "call %d", call)
			return ""
		}
	})
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: llm.URL, Model: "test-model"}

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
				`{"id":"c1","type":"function","function":{"name":"load_media","arguments":"{\"name\":\"pic.png\"}"}}]}}]}`
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
				`{"id":"c2","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"screenshot\"}"}},` +
				`{"id":"c3","type":"function","function":{"name":"finish","arguments":"{}"}}]}}]}`
		default:
			rq.Failf("unexpected extra LLM call", "call %d", call)
			return ""
		}
	})
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: srv.URL, Model: "test-model"}

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
				`{"id":"c1","type":"function","function":{"name":"load_media","arguments":"{\"name\":\"a.png\"}"}},` +
				`{"id":"c2","type":"function","function":{"name":"load_media","arguments":"{\"name\":\"b.png\"}"}}]}}]}`
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
				`{"id":"c3","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"photos\"}"}},` +
				`{"id":"c4","type":"function","function":{"name":"finish","arguments":"{}"}}]}}]}`
		default:
			rq.Failf("unexpected extra LLM call", "call %d", call)
			return ""
		}
	})
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: srv.URL, Model: "test-model"}
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
			// Stopping without finish nudges the model and continues with the
			// full tool set.
			rq.Len(req.Tools, 8)
			nudged := false
			for _, m := range req.Messages {
				if s, ok := m.Content.(string); ok && m.Role == "user" && strings.Contains(s, "call finish") {
					nudged = true
				}
			}
			rq.True(nudged)
		}
		if call == analyzeBaseRounds+1 {
			// The round budget is spent: the forced wrap-up round drops the
			// read tools.
			rq.Len(req.Tools, 5)
		}
		// The model never calls a tool and answers in plain text.
		return `{"choices":[{"message":{"role":"assistant","content":"weekly-report"}}]}`
	})
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: srv.URL, Model: "test-model"}
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
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: srv.URL, Model: "test-model"}
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
				`{"id":"c","type":"function","function":{"name":"read_file","arguments":"{\"name\":\"a.txt\"}"}}]}}]}`
		}
		rq.Equal(analyzeBaseRounds+1, call)
		rq.Len(req.Tools, 5, "forced round drops the read tools")
		nudged := false
		for _, m := range req.Messages {
			if s, ok := m.Content.(string); ok && m.Role == "user" && strings.Contains(s, "wrap up now") {
				nudged = true
			}
		}
		rq.True(nudged)
		return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
			`{"id":"c2","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"forced\"}"}}]}}]}`
	})
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: srv.URL, Model: "test-model"}
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
	// The staged binary would trigger a best-effort conversion; keep the
	// converters stubbed out.
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called")
		return nil, nil
	})
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
		// Native text gets a line count and a one-line peek; the binary file
		// gets neither and is listed with no readable form.
		rq.Contains(list, "`report.txt` (30 B, text, 3 lines)")
		rq.Contains(list, "peek: Q3 revenue summary and outlook")
		rq.Equal(1, strings.Count(list, "peek:"))
		rq.Contains(list, "`bin.dat` (3 B, binary) — no readable form")
		return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"q3-report\"}"}},` +
			`{"id":"c2","type":"function","function":{"name":"finish","arguments":"{}"}}]}}]}`
	})
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: srv.URL, Model: "test-model"}
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
			`{"id":"c1","type":"function","function":{"name":"set_title","arguments":{"title":"from-object"}}},` +
			`{"id":"c2","type":"function","function":{"name":"finish","arguments":"{}"}}]}}]}`
	})
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: srv.URL, Model: "test-model"}
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
				`{"id":"c2","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"invoice\"}"}},` +
				`{"id":"c3","type":"function","function":{"name":"finish","arguments":"{}"}}]}}]}`
		default:
			rq.Failf("unexpected extra LLM call", "call %d", call)
			return ""
		}
	})
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: srv.URL, Model: "test-model"}
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
				`{"id":"c2","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"ok\"}"}},` +
				`{"id":"c3","type":"function","function":{"name":"finish","arguments":"{}"}}]}}]}`
		default:
			rq.Failf("unexpected extra LLM call", "call %d", call)
			return ""
		}
	})
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: srv.URL, Model: "test-model"}
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
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: srv.URL, Model: "test-model"}
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
			`{"id":"c1","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"ok\"}"}},` +
			`{"id":"c2","type":"function","function":{"name":"finish","arguments":"{}"}}]}}]}`
	})
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: llm.URL, Model: "test-model"}
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

func TestClassifyStagedFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
	}
	write("report.pdf", []byte("not really a pdf but text"))  // extension wins
	write("scan.svg", []byte("<svg>text but an image</svg>")) // extension wins
	write("photo.heic", []byte{0, 1, 2})
	write("clip.mp4", []byte("fake"))
	write("notes.txt", []byte("hello"))
	write("readme", []byte("no extension, still text"))
	write("bin.dat", []byte{'x', 0, 'y'})
	write("empty.md", nil)

	cases := []struct {
		name string
		kind analyzeKind
	}{
		{"report.pdf", kindDocument},
		{"scan.svg", kindImage},
		{"photo.heic", kindImage},
		{"clip.mp4", kindVideo},
		{"notes.txt", kindText},
		{"readme", kindText},
		{"bin.dat", kindOther},
		{"empty.md", kindText},
		{"missing.txt", kindOther},
	}
	for _, tc := range cases {
		require.Equal(t, tc.kind, classifyStagedFile(dir, tc.name), tc.name)
	}
}

func TestPrepAnalyzeDocument(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "report.docx"), []byte("PK\x03\x04binary"), 0o644))

	// A document pre-converts both ways: full text plus rendered pages.
	stubConvert(t, func(name string) (string, error) {
		switch name {
		case "soffice", "pdftoppm":
			return "/usr/bin/" + name, nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case strings.Contains(name, "soffice"):
			outdir := outdirArg(t, args)
			format := "txt"
			for i, a := range args {
				if a == "--convert-to" && i+1 < len(args) {
					format = args[i+1]
				}
			}
			if format == "pdf" {
				return nil, os.WriteFile(filepath.Join(outdir, "report.pdf"), []byte("%PDF-1.7"), 0o644)
			}
			return nil, os.WriteFile(filepath.Join(outdir, "report.txt"), []byte("line one\nline two\n"), 0o644)
		case strings.Contains(name, "pdftoppm"):
			fakePdftoppm(t, args, 2)
			return nil, nil
		}
		t.Fatalf("unexpected command %s", name)
		return nil, nil
	})

	files, err := listWorkspaceFiles(dir)
	require.NoError(t, err)
	entries := prepAnalyze(context.Background(), dir, files, nil)
	require.Len(t, entries, 1)
	e := entries[0]
	require.Equal(t, kindDocument, e.Kind)
	require.False(t, e.Failed)
	require.Equal(t, "report.docx.txt", e.Text)
	require.Equal(t, 2, e.TextLines)
	require.Equal(t, []string{"report.docx.p01.jpg", "report.docx.p02.jpg"}, e.Pages)

	// The products are written into .filestor/analyze under the derived names.
	text, err := os.ReadFile(filepath.Join(workspaceAnalyzePath(dir), "report.docx.txt"))
	require.NoError(t, err)
	require.Equal(t, "line one\nline two\n", string(text))
	_, err = os.Stat(filepath.Join(workspaceAnalyzePath(dir), "report.docx.p01.jpg"))
	require.NoError(t, err)
	listing := buildAnalyzeListing(entries)
	require.Contains(t, listing, "- `report.docx` (10 B, document)\n")
	require.Contains(t, listing, "  text: `report.docx.txt` (2 lines)\n")
	require.Contains(t, listing, "  pages: `report.docx.p01.jpg` `report.docx.p02.jpg`\n")
}

func TestPrepAnalyzeScannedDocument(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "scan.pdf"), []byte("%PDF-1.7 fake"), 0o644))

	// A scanned PDF yields whitespace-only text but renders page images.
	stubConvert(t, func(name string) (string, error) {
		switch name {
		case "pdftotext", "pdftoppm":
			return "/usr/bin/" + name, nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case strings.Contains(name, "pdftotext"):
			return []byte("  \n "), nil
		case strings.Contains(name, "pdftoppm"):
			fakePdftoppm(t, args, 2)
			return nil, nil
		}
		t.Fatalf("unexpected command %s", name)
		return nil, nil
	})

	files, err := listWorkspaceFiles(dir)
	require.NoError(t, err)
	entries := prepAnalyze(context.Background(), dir, files, nil)
	require.Len(t, entries, 1)
	e := entries[0]
	// Whitespace-only text is not a text form; the pages carry the content.
	require.Empty(t, e.Text)
	require.Equal(t, []string{"scan.pdf.p01.jpg", "scan.pdf.p02.jpg"}, e.Pages)
	require.False(t, e.Failed)
	_, err = os.Stat(filepath.Join(workspaceAnalyzePath(dir), "scan.pdf.p01.jpg"))
	require.NoError(t, err)
}

func TestPrepAnalyzeVideo(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "scan.mp4"), []byte("fake mp4"), 0o644))

	stubConvert(t, func(name string) (string, error) {
		if name == "ffmpeg" {
			return "/usr/bin/ffmpeg", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		require.Contains(t, name, "ffmpeg")
		pattern := args[len(args)-1]
		for i := 1; i <= 3; i++ {
			require.NoError(t, os.WriteFile(fmt.Sprintf(pattern, i), []byte{0xff, 0xd8, 0xff, byte(i)}, 0o644))
		}
		return nil, nil
	})

	files, err := listWorkspaceFiles(dir)
	require.NoError(t, err)
	entries := prepAnalyze(context.Background(), dir, files, nil)
	require.Len(t, entries, 1)
	e := entries[0]
	require.Equal(t, kindVideo, e.Kind)
	require.False(t, e.Failed)
	require.Equal(t, []string{"scan.mp4.f1.jpg", "scan.mp4.f2.jpg", "scan.mp4.f3.jpg"}, e.Frames)
	_, err = os.Stat(filepath.Join(workspaceAnalyzePath(dir), "scan.mp4.f3.jpg"))
	require.NoError(t, err)
	require.Contains(t, buildAnalyzeListing(entries), "  frames: `scan.mp4.f1.jpg` `scan.mp4.f2.jpg` `scan.mp4.f3.jpg`\n")
}

func TestPrepAnalyzeVideoNoFFmpeg(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "scan.mp4"), []byte("fake mp4"), 0o644))
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called")
		return nil, nil
	})
	files, err := listWorkspaceFiles(dir)
	require.NoError(t, err)
	entries := prepAnalyze(context.Background(), dir, files, nil)
	require.True(t, entries[0].Failed)
	require.Contains(t, buildAnalyzeListing(entries), "video) — conversion failed")
}

func TestPrepAnalyzeImages(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	png := append(append([]byte{}, pngSig...), []byte("rest")...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pic.png"), png, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "photo.heic"), []byte("fake heic"), 0o644))
	// Bigger than 8 MiB so the native jpeg path is skipped.
	huge := make([]byte, analyzeImageMaxBytes+1)
	huge[0], huge[1], huge[2] = 0xff, 0xd8, 0xff
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.jpg"), huge, 0o644))

	stubConvert(t, func(name string) (string, error) {
		if name == "magick" {
			return "/usr/bin/magick", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		require.Contains(t, name, "magick")
		out := args[len(args)-1]
		return nil, os.WriteFile(out, []byte{0xff, 0xd8, 0xff, 0xd9}, 0o644)
	})

	files, err := listWorkspaceFiles(dir)
	require.NoError(t, err)
	entries := prepAnalyze(context.Background(), dir, files, nil)
	require.Len(t, entries, 3)
	byName := map[string]*analyzeEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	// A small native png needs no conversion and is not a failure.
	require.Equal(t, kindImage, byName["pic.png"].Kind)
	require.Empty(t, byName["pic.png"].Image)
	require.False(t, byName["pic.png"].Failed)
	// Non-native and oversized images get a normalized jpeg.
	require.Equal(t, "photo.heic.jpg", byName["photo.heic"].Image)
	require.Equal(t, "big.jpg.jpg", byName["big.jpg"].Image)
	_, err = os.Stat(filepath.Join(workspaceAnalyzePath(dir), "photo.heic.jpg"))
	require.NoError(t, err)
}

func TestPrepAnalyzeOtherBestEffort(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "model.stl"), []byte{'x', 0, 'y'}, 0o644))
	// Larger than the best-effort cap: never even attempted.
	huge := make([]byte, otherConvertMaxBytes+1)
	huge[0] = 0
	require.NoError(t, os.WriteFile(filepath.Join(dir, "huge.bin"), huge, 0o644))

	stubConvert(t, func(name string) (string, error) {
		if name == "soffice" {
			return "/usr/bin/soffice", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		src := args[len(args)-1]
		require.Contains(t, src, "model.stl", "huge.bin must be skipped, not converted")
		return nil, os.WriteFile(filepath.Join(outdirArg(t, args), "model.txt"), []byte("solid cube"), 0o644)
	})

	files, err := listWorkspaceFiles(dir)
	require.NoError(t, err)
	entries := prepAnalyze(context.Background(), dir, files, nil)
	require.Len(t, entries, 2)
	byName := map[string]*analyzeEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	require.Equal(t, "model.stl.txt", byName["model.stl"].Text)
	require.Empty(t, byName["huge.bin"].Text)
	listing := buildAnalyzeListing(entries)
	require.Contains(t, listing, "  text: `model.stl.txt` (1 lines)\n")
	require.Contains(t, listing, "`huge.bin` (64.0 MB, binary) — no readable form")
}

func TestPrepAnalyzeFailureDoesNotAbort(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.docx"), []byte("PK broken"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "good.docx"), []byte("PK good"), 0o644))

	stubConvert(t, func(name string) (string, error) {
		if name == "soffice" {
			return "/usr/bin/soffice", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		require.Contains(t, name, "soffice")
		src := args[len(args)-1]
		if strings.Contains(src, "broken") {
			return nil, errors.New("boom")
		}
		format := "txt"
		for i, a := range args {
			if a == "--convert-to" && i+1 < len(args) {
				format = args[i+1]
			}
		}
		outdir := outdirArg(t, args)
		if format == "pdf" {
			return nil, errors.New("no pages in this test")
		}
		return nil, os.WriteFile(filepath.Join(outdir, "good.txt"), []byte("good body"), 0o644)
	})

	files, err := listWorkspaceFiles(dir)
	require.NoError(t, err)
	entries := prepAnalyze(context.Background(), dir, files, nil)
	require.Len(t, entries, 2)
	byName := map[string]*analyzeEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	// The broken file is marked but the good one still converted.
	require.True(t, byName["broken.docx"].Failed)
	require.Equal(t, "good.docx.txt", byName["good.docx"].Text)
	require.Contains(t, buildAnalyzeListing(entries), "`broken.docx` (9 B, document) — conversion failed")
}

// newTestAgent builds an analyzeAgent over a temp workspace with the entries
// indexed, without running an LLM.
func newTestAgent(t *testing.T, dir string, entries []*analyzeEntry) *analyzeAgent {
	t.Helper()
	a := &analyzeAgent{dir: dir}
	a.indexEntries(entries)
	return a
}

func TestReadFileTool(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&sb, "line %04d\n", i)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(sb.String()), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644))
	// A derived text form lives under .filestor/analyze.
	require.NoError(t, os.MkdirAll(workspaceAnalyzePath(dir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceAnalyzePath(dir), "report.docx.txt"), []byte("doc body\n"), 0o644))

	a := newTestAgent(t, dir, []*analyzeEntry{
		{Name: "notes.txt", Kind: kindText},
		{Name: "empty.txt", Kind: kindText},
		{Name: "report.docx", Kind: kindDocument, Text: "report.docx.txt", TextLines: 1},
		{Name: "pic.png", Kind: kindImage},
	})

	// Default page: first 200 lines.
	out := a.toolReadFile("notes.txt", 0, 0)
	require.Contains(t, out, "`notes.txt`: lines 1-200 of 1000\n")
	require.Contains(t, out, "line 0001")
	require.Contains(t, out, "line 0200")
	require.NotContains(t, out, "line 0201")

	// Paging with offset and limit.
	out = a.toolReadFile("notes.txt", 990, 50)
	require.Contains(t, out, "`notes.txt`: lines 990-1000 of 1000\n")
	require.Contains(t, out, "line 1000")

	// Limit is capped at 500.
	out = a.toolReadFile("notes.txt", 1, 9999)
	require.Contains(t, out, "`notes.txt`: lines 1-500 of 1000\n")

	// Offset past the end.
	out = a.toolReadFile("notes.txt", 5000, 10)
	require.Contains(t, out, "offset 5000 is past the end")

	// Empty file.
	out = a.toolReadFile("empty.txt", 1, 10)
	require.Contains(t, out, "is empty (0 lines)")

	// The derived text form reads from .filestor/analyze.
	out = a.toolReadFile("report.docx.txt", 1, 10)
	require.Contains(t, out, "`report.docx.txt`: lines 1-1 of 1\ndoc body")

	// A document source name errors and points at its text form.
	out = a.toolReadFile("report.docx", 1, 10)
	require.Contains(t, out, "error:")
	require.Contains(t, out, "`report.docx.txt`")

	// An image source name points at load_media.
	out = a.toolReadFile("pic.png", 1, 10)
	require.Contains(t, out, "error:")
	require.Contains(t, out, "load_media")

	// Unknown names and traversal fail.
	require.Contains(t, a.toolReadFile("nope.txt", 1, 10), "no such file")
	require.Contains(t, a.toolReadFile("../secret", 1, 10), "error:")
}

func TestReadFileToolCaps(t *testing.T) {
	dir := t.TempDir()
	// 600 lines of 200 chars: a 500-line page exceeds the 64 KiB reply cap,
	// so trailing lines are dropped and the header says so.
	var sb strings.Builder
	for i := 1; i <= 600; i++ {
		fmt.Fprintf(&sb, "row %04d %s\n", i, strings.Repeat("x", 190))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wide.txt"), []byte(sb.String()), 0o644))

	// A staging text file larger than readFileMaxBytes is flagged truncated.
	big := append([]byte(strings.Repeat("a", readFileMaxBytes)), []byte("\ntail")...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.txt"), big, 0o644))

	a := newTestAgent(t, dir, []*analyzeEntry{
		{Name: "wide.txt", Kind: kindText},
		{Name: "big.txt", Kind: kindText},
	})

	out := a.toolReadFile("wide.txt", 1, 500)
	require.Contains(t, out, "reply capped at 64.0 KB")
	body := out[strings.IndexByte(out, '\n')+1:]
	require.LessOrEqual(t, len(body), analyzeTextMaxBytes)

	out = a.toolReadFile("big.txt", 1, 1)
	require.Contains(t, out, "only the first 8.0 MB of the file are readable")
}

func TestLoadMediaTool(t *testing.T) {
	dir := t.TempDir()
	png := append(append([]byte{}, pngSig...), []byte("rest")...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pic.png"), png, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644))
	require.NoError(t, os.MkdirAll(workspaceAnalyzePath(dir), 0o755))
	jpg := []byte{0xff, 0xd8, 0xff, 0xd9}
	for _, n := range []string{"report.docx.p01.jpg", "report.docx.p02.jpg", "clip.mp4.f1.jpg", "photo.heic.jpg"} {
		require.NoError(t, os.WriteFile(filepath.Join(workspaceAnalyzePath(dir), n), jpg, 0o644))
	}

	a := newTestAgent(t, dir, []*analyzeEntry{
		{Name: "pic.png", Kind: kindImage},
		{Name: "photo.heic", Kind: kindImage, Image: "photo.heic.jpg"},
		{Name: "report.docx", Kind: kindDocument, Text: "report.docx.txt", Pages: []string{"report.docx.p01.jpg", "report.docx.p02.jpg"}},
		{Name: "clip.mp4", Kind: kindVideo, Frames: []string{"clip.mp4.f1.jpg"}},
		{Name: "notes.txt", Kind: kindText},
		{Name: "model.stl", Kind: kindOther},
	})

	// A small native image loads straight from staging with its real mime.
	label, imgs := a.toolLoadMedia("pic.png")
	require.Equal(t, "loaded `pic.png`", label)
	require.Len(t, imgs, 1)
	require.Equal(t, "image/png", imgs[0].mime)

	// A normalized image loads from .filestor/analyze.
	label, imgs = a.toolLoadMedia("photo.heic")
	require.Equal(t, "loaded `photo.heic.jpg`", label)
	require.Len(t, imgs, 1)
	require.Equal(t, "image/jpeg", imgs[0].mime)

	// A document loads all its pages at once.
	label, imgs = a.toolLoadMedia("report.docx")
	require.Equal(t, "loaded 2 page(s) of `report.docx`", label)
	require.Len(t, imgs, 2)

	// A video loads its frames.
	label, imgs = a.toolLoadMedia("clip.mp4")
	require.Equal(t, "loaded 1 frame(s) of `clip.mp4`", label)
	require.Len(t, imgs, 1)

	// A single derived image name loads just that image.
	label, imgs = a.toolLoadMedia("report.docx.p02.jpg")
	require.Equal(t, "loaded `report.docx.p02.jpg`", label)
	require.Len(t, imgs, 1)

	// Errors: text goes to read_file, unknown names fail, binaries have no
	// image form.
	label, imgs = a.toolLoadMedia("notes.txt")
	require.Contains(t, label, "read_file")
	require.Empty(t, imgs)
	label, _ = a.toolLoadMedia("report.docx.txt")
	require.Contains(t, label, "read_file")
	label, imgs = a.toolLoadMedia("nope.jpg")
	require.Contains(t, label, "no such file")
	require.Empty(t, imgs)
	label, _ = a.toolLoadMedia("model.stl")
	require.Contains(t, label, "no image form")
	label, _ = a.toolLoadMedia("../secret")
	require.Contains(t, label, "error")
}

func TestRunToolLoadMediaMultiParts(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(workspaceAnalyzePath(dir), 0o755))
	jpg := []byte{0xff, 0xd8, 0xff, 0xd9}
	require.NoError(t, os.WriteFile(filepath.Join(workspaceAnalyzePath(dir), "doc.pdf.p01.jpg"), jpg, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceAnalyzePath(dir), "doc.pdf.p02.jpg"), jpg, 0o644))

	a := newTestAgent(t, dir, []*analyzeEntry{
		{Name: "doc.pdf", Kind: kindDocument, Pages: []string{"doc.pdf.p01.jpg", "doc.pdf.p02.jpg"}},
	})
	// All pages of a document ride in one user message with one image part
	// per page, after the tool reply.
	reply, extra := a.runTool(context.Background(), chatToolCall{
		ID: "c1", Type: "function",
		Function: chatFunctionCall{Name: "load_media", Arguments: json.RawMessage(`{"name":"doc.pdf"}`)},
	})
	require.Equal(t, "tool", reply.Role)
	require.Equal(t, "c1", reply.ToolCallID)
	require.Equal(t, "loaded 2 page(s) of `doc.pdf`", reply.Content)
	require.Len(t, extra, 1)
	parts, ok := extra[0].Content.([]chatPart)
	require.True(t, ok)
	require.Len(t, parts, 2)
	for _, p := range parts {
		require.Equal(t, "image_url", p.Type)
		require.True(t, strings.HasPrefix(p.ImageURL.URL, "data:image/jpeg;base64,"))
	}
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
			cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: llm.URL, Model: "m"}
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

func TestUploadAnalyzeDigest(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world"), 0o644))
	png := append(append([]byte{}, pngSig...), []byte("rest")...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pic.png"), png, 0o644))

	srv := newFakeLLM(t, func(rq *require.Assertions, call int, r *http.Request, req chatRequest) string {
		rq.Equal(1, call)
		return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"mark_text","arguments":"{\"text\":\"hello world overview\"}"}},` +
			`{"id":"c2","type":"function","function":{"name":"mark_image","arguments":"{\"name\":\"pic.png\"}"}},` +
			`{"id":"c3","type":"function","function":{"name":"set_title","arguments":"{\"title\":\"greeting\"}"}},` +
			`{"id":"c4","type":"function","function":{"name":"finish","arguments":"{}"}}]}}]}`
	})
	cfg.LLM.Chat.OpenAI = OpenAIChatConfig{URL: srv.URL, Model: "test-model"}
	app := NewServer(cfg, &fakeStore{})
	h := app.Handler()
	cookie := loginCookie(t, h)
	require.Equal(t, http.StatusAccepted, postAnalyze(t, h, cookie).Code)
	awaitIdle(t, app)
	require.Equal(t, "greeting", loadWorkspaceState(dir).Title)

	// The marks landed in .filestor/digest: one txt per text chunk, one file
	// per image.
	data, err := os.ReadFile(filepath.Join(workspaceDigestPath(dir), "text-01.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello world overview", string(data))
	data, err = os.ReadFile(filepath.Join(workspaceDigestPath(dir), "image-01-pic.png"))
	require.NoError(t, err)
	require.Equal(t, png, data)
}

func TestMarkTextTool(t *testing.T) {
	dir := t.TempDir()
	a := newTestAgent(t, dir, nil)

	require.Equal(t, "marked text chunk 1/16", a.toolMarkText("first chunk"))
	require.Equal(t, "marked text chunk 2/16", a.toolMarkText(" second chunk "))

	data, err := os.ReadFile(filepath.Join(workspaceDigestPath(dir), "text-01.txt"))
	require.NoError(t, err)
	require.Equal(t, "first chunk", string(data))
	data, err = os.ReadFile(filepath.Join(workspaceDigestPath(dir), "text-02.txt"))
	require.NoError(t, err)
	require.Equal(t, "second chunk", string(data))

	// Empty chunks are refused.
	require.Contains(t, a.toolMarkText("  "), "error:")

	// An oversized chunk is truncated to the per-chunk cap.
	require.Equal(t, "marked text chunk 3/16", a.toolMarkText(strings.Repeat("x", analyzeDigestTextMaxBytes+100)))
	data, err = os.ReadFile(filepath.Join(workspaceDigestPath(dir), "text-03.txt"))
	require.NoError(t, err)
	require.Len(t, data, analyzeDigestTextMaxBytes)

	// The cap refuses further chunks.
	a.digestTexts = analyzeDigestMaxTexts
	require.Contains(t, a.toolMarkText("one more"), "no more can be marked")
}

func TestMarkImageTool(t *testing.T) {
	dir := t.TempDir()
	png := append(append([]byte{}, pngSig...), []byte("rest")...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pic.png"), png, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644))
	require.NoError(t, os.MkdirAll(workspaceAnalyzePath(dir), 0o755))
	jpg := []byte{0xff, 0xd8, 0xff, 0xd9}
	for _, n := range []string{"report.docx.p01.jpg", "report.docx.p02.jpg", "clip.mp4.f1.jpg", "photo.heic.jpg"} {
		require.NoError(t, os.WriteFile(filepath.Join(workspaceAnalyzePath(dir), n), jpg, 0o644))
	}

	a := newTestAgent(t, dir, []*analyzeEntry{
		{Name: "pic.png", Kind: kindImage},
		{Name: "photo.heic", Kind: kindImage, Image: "photo.heic.jpg"},
		{Name: "report.docx", Kind: kindDocument, Pages: []string{"report.docx.p01.jpg", "report.docx.p02.jpg"}},
		{Name: "clip.mp4", Kind: kindVideo, Frames: []string{"clip.mp4.f1.jpg"}},
		{Name: "notes.txt", Kind: kindText},
		{Name: "model.stl", Kind: kindOther},
	})

	// A native staged image copies straight from staging.
	require.Equal(t, "marked `pic.png` as digest image 1/4", a.toolMarkImage("pic.png"))
	data, err := os.ReadFile(filepath.Join(workspaceDigestPath(dir), "image-01-pic.png"))
	require.NoError(t, err)
	require.Equal(t, png, data)

	// A single derived image name, and a converted image by its source name.
	require.Equal(t, "marked `report.docx.p01.jpg` as digest image 2/4", a.toolMarkImage("report.docx.p01.jpg"))
	require.Equal(t, "marked `photo.heic.jpg` as digest image 3/4", a.toolMarkImage("photo.heic"))
	data, err = os.ReadFile(filepath.Join(workspaceDigestPath(dir), "image-03-photo.heic.jpg"))
	require.NoError(t, err)
	require.Equal(t, jpg, data)

	// A repeated mark is not written twice.
	require.Equal(t, "`pic.png` is already marked", a.toolMarkImage("pic.png"))

	// A document/video source name stands for several images: refused with a
	// pointer at the derived names.
	out := a.toolMarkImage("report.docx")
	require.Contains(t, out, "error:")
	require.Contains(t, out, "report.docx.p01.jpg")
	require.Contains(t, a.toolMarkImage("clip.mp4"), "error:")

	// Text, unknown names, and binaries fail.
	require.Contains(t, a.toolMarkImage("notes.txt"), "mark_text")
	require.Contains(t, a.toolMarkImage("nope.jpg"), "no such file")
	require.Contains(t, a.toolMarkImage("model.stl"), "no image form")
	require.Contains(t, a.toolMarkImage("../secret"), "error:")

	// The cap refuses further images.
	require.Equal(t, "marked `clip.mp4.f1.jpg` as digest image 4/4", a.toolMarkImage("clip.mp4.f1.jpg"))
	require.Contains(t, a.toolMarkImage("report.docx.p02.jpg"), "no more can be marked")
}

func TestPrepAnalyzeResetsDigest(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	dir := cfg.Upload.Workspace
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644))
	require.NoError(t, os.MkdirAll(workspaceDigestPath(dir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDigestPath(dir), "text-01.txt"), []byte("stale"), 0o644))

	files, err := listWorkspaceFiles(dir)
	require.NoError(t, err)
	prepAnalyze(context.Background(), dir, files, nil)

	// The digest directory is rebuilt empty for the new run.
	entries, err := os.ReadDir(workspaceDigestPath(dir))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestRenameFileToolRenamesDerived(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "IMG_2048.pdf"), []byte("%PDF fake"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644))
	require.NoError(t, os.MkdirAll(workspaceAnalyzePath(dir), 0o755))
	jpg := []byte{0xff, 0xd8, 0xff, 0xd9}
	require.NoError(t, os.WriteFile(filepath.Join(workspaceAnalyzePath(dir), "IMG_2048.pdf.txt"), []byte("doc body"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceAnalyzePath(dir), "IMG_2048.pdf.p01.jpg"), jpg, 0o644))

	a := newTestAgent(t, dir, []*analyzeEntry{
		{Name: "IMG_2048.pdf", Size: "10 B", Kind: kindDocument, Text: "IMG_2048.pdf.txt", TextLines: 1, Pages: []string{"IMG_2048.pdf.p01.jpg"}},
		{Name: "notes.txt", Size: "2 B", Kind: kindText},
	})

	reply, extra := a.runTool(context.Background(), chatToolCall{
		ID: "c1", Type: "function",
		Function: chatFunctionCall{Name: "rename_file", Arguments: json.RawMessage(`{"name":"IMG_2048.pdf","new_name":"invoice-2026-03.pdf"}`)},
	})
	require.Empty(t, extra)
	text, _ := reply.Content.(string)
	require.Contains(t, text, "renamed to `invoice-2026-03.pdf`")
	// The reply carries the complete updated roster with the new names.
	require.Contains(t, text, "invoice-2026-03.pdf.txt")
	require.Contains(t, text, "invoice-2026-03.pdf.p01.jpg")
	require.Contains(t, text, "notes.txt")
	require.NotContains(t, text, "IMG_2048")

	// The staged file and its derived products were renamed on disk.
	_, err := os.Stat(filepath.Join(dir, "invoice-2026-03.pdf"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(workspaceAnalyzePath(dir), "invoice-2026-03.pdf.txt"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(workspaceAnalyzePath(dir), "invoice-2026-03.pdf.p01.jpg"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(workspaceAnalyzePath(dir), "IMG_2048.pdf.p01.jpg"))
	require.True(t, os.IsNotExist(err))

	// The entry and the name lookups follow the new names.
	e := a.entries["invoice-2026-03.pdf"]
	require.NotNil(t, e)
	require.Equal(t, "invoice-2026-03.pdf.txt", e.Text)
	require.Equal(t, []string{"invoice-2026-03.pdf.p01.jpg"}, e.Pages)
	require.Nil(t, a.entries["IMG_2048.pdf"])
	require.Same(t, e, a.derived["invoice-2026-03.pdf.p01.jpg"])
	require.Nil(t, a.derived["IMG_2048.pdf.p01.jpg"])

	// Later read/load calls resolve by the new names.
	out := a.toolReadFile("invoice-2026-03.pdf.txt", 1, 10)
	require.Contains(t, out, "doc body")
	label, imgs := a.toolLoadMedia("invoice-2026-03.pdf")
	require.Contains(t, label, "1 page(s) of `invoice-2026-03.pdf`")
	require.Len(t, imgs, 1)
}

func TestRunToolReadsClosed(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644))
	a := newTestAgent(t, dir, []*analyzeEntry{{Name: "a.txt", Kind: kindText}})
	a.readsClosed = true

	// The wrap-up round offers no read tools; calling one anyway is refused.
	for _, tool := range []string{"read_file", "load_media", "rename_file"} {
		reply, _ := a.runTool(context.Background(), chatToolCall{
			ID: "c1", Type: "function",
			Function: chatFunctionCall{Name: tool, Arguments: json.RawMessage(`{"name":"a.txt","new_name":"b.txt"}`)},
		})
		require.Contains(t, reply.Content, "reading is closed", tool)
	}

	// The output tools still work while reads are closed.
	reply, _ := a.runTool(context.Background(), chatToolCall{
		ID: "c2", Type: "function",
		Function: chatFunctionCall{Name: "mark_text", Arguments: json.RawMessage(`{"text":"chunk"}`)},
	})
	require.Equal(t, "marked text chunk 1/16", reply.Content)
}
