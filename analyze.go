package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	analyzeBaseRounds       = 16
	analyzeRoundsPerFile    = 1 // per staged file beyond the first
	analyzeRoundsCap        = 64
	analyzeBaseToolCalls    = 24
	analyzeToolCallsPerFile = 2 // a read plus an occasional rename per extra file
	analyzeToolCallsCap     = 256
	analyzeTextMaxBytes     = 64 << 10
	analyzeImageMaxBytes    = 8 << 20
	analyzePeekMaxBytes     = 1 << 10
	analyzeHTTPTimeout      = 120 * time.Second
)

// analyzeBudget scales the round and tool-call budgets with the staged file
// count: a single file gets the base budget, every extra file adds a bit more
// (bounded by the caps), so a large batch has room to read and rename each
// file without exhausting the loop.
func analyzeBudget(files int) (rounds, toolCalls int) {
	extra := files - 1
	if extra < 0 {
		extra = 0
	}
	rounds = min(analyzeBaseRounds+analyzeRoundsPerFile*extra, analyzeRoundsCap)
	toolCalls = min(analyzeBaseToolCalls+analyzeToolCallsPerFile*extra, analyzeToolCallsCap)
	return rounds, toolCalls
}

// analyzeSystemPromptRaw uses ⟪⟫ as stand-ins for backticks: a Go raw string
// cannot contain a literal backtick, and the prompt quotes file names in
// `backticks` so the model can tell them apart from other text.
const analyzeSystemPromptRaw = `You name a batch of staged files for upload: decide one short, descriptive title for the whole batch, and optionally the document datetime.
- The user message lists each staged file with its size, plus a one-line "peek" at each text file's leading content. Names and peeks are often enough — use the read tools only when you need more.
- Skip very large files (e.g. a PDF of several hundred MB): judge them by name instead of reading or converting them; a huge file must not block your decision.
- read_file_as_text and read_file_as_image convert office documents, PDFs, and other formats automatically; you do not need to care about the conversion.
- The title is a short phrase (at most 40 characters) in the same language as the content, e.g. "weekly-report" or "月度账单".
- If the contents contain a clear document date or datetime, call set_datetime with it (YYYY-MM-DD or YYYY-MM-DDTHH:mm), before or in the same turn as set_title. Do not guess.
- If a staged file's name is clearly messy or uninformative — camera/scanner codes like ⟪IMG_2048.jpg⟫ or ⟪SCAN_0001.pdf⟫, timestamp-only screenshot names, random hashes, placeholder names like ⟪untitled⟫ or ⟪新建文档⟫, noise like ⟪final2⟫, ⟪copy of⟫, ⟪(1)⟫, or a name date that is redundant or contradicts the document's actual date — and you are confident about its content, call rename_file with a short, descriptive new name in the same language as the content. Use the document's actual date in the new name or drop the date entirely; never keep a date you know is wrong. Keep the extension; use only letters, digits, dash, underscore, dot; rename each file at most once; never pick a name another staged file already has. When in doubt, keep the original name — renaming is optional and must not delay set_title.
- Call set_title exactly once with the raw title. Decide quickly: reading every file is rarely necessary. File names in messages are wrapped in ⟪backticks⟫; tool arguments take the bare name without backticks.`

var analyzeSystemPrompt = strings.NewReplacer("⟪", "`", "⟫", "`").Replace(analyzeSystemPromptRaw)

type chatImageURL struct {
	URL string `json:"url"`
}

type chatPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

// chatMessage covers the roles we send: system/user (string or parts content),
// assistant (content + tool_calls), and tool (content + tool_call_id).
type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type chatTool struct {
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatRequest struct {
	Model           string        `json:"model"`
	Messages        []chatMessage `json:"messages"`
	Tools           []chatTool    `json:"tools,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
}

type chatReplyMessage struct {
	Content   string         `json:"content"`
	ToolCalls []chatToolCall `json:"tool_calls"`
}

type chatResponse struct {
	Choices []struct {
		Message chatReplyMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func nameParamTool(name, description string) chatTool {
	return chatTool{Type: "function", Function: chatToolFunction{
		Name:        name,
		Description: description,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Staged file name, without backticks"},
			},
			"required": []string{"name"},
		},
	}}
}

var analyzeTools = []chatTool{
	nameParamTool("read_file_as_text", "Read a staged file as text; office documents and PDFs are converted automatically."),
	nameParamTool("read_file_as_image", "Load a staged file as an image to view it; oversized or non-jpeg/png/gif files are converted automatically."),
	{Type: "function", Function: chatToolFunction{
		Name:        "rename_file",
		Description: "Rename a staged file whose name is messy, uninformative, or carries a redundant or wrong date. Only when confident about the content; keep the extension unchanged.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":     map[string]any{"type": "string", "description": "Staged file name, without backticks"},
				"new_name": map[string]any{"type": "string", "description": "Short, descriptive new file name with the original extension"},
			},
			"required": []string{"name", "new_name"},
		},
	}},
	{Type: "function", Function: chatToolFunction{
		Name:        "set_title",
		Description: "Set the upload title. Call exactly once when you have decided.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title": map[string]any{"type": "string", "description": "The raw title phrase"},
			},
			"required": []string{"title"},
		},
	}},
	{Type: "function", Function: chatToolFunction{
		Name:        "set_datetime",
		Description: "Set the bundle time from a clear date or datetime found in the files. Do not guess.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"time": map[string]any{"type": "string", "description": "Document time as YYYY-MM-DDTHH:mm, or YYYY-MM-DD if only a date is present"},
			},
			"required": []string{"time"},
		},
	}},
}

// analyzeDecisionTools are the only tools offered when forcing a decision
// after the read budget is spent (set_title and set_datetime).
var analyzeDecisionTools = analyzeTools[3:]

// analyzeAgent runs the chat-completions tool loop against the configured
// OpenAI-compatible endpoint.
type analyzeAgent struct {
	cfg          LLMConfig
	dir          string
	http         *http.Client
	maxRounds    int
	maxToolCalls int
	title        string
	when         string
	readsClosed  bool
	onProgress   func(jobProgress)
	onState      func()
	onFiles      func()
}

func (s *Server) handleUploadAnalyze(w http.ResponseWriter, r *http.Request) {
	if s.Config == nil || s.Config.LLM.URL == "" {
		http.Error(w, "llm not configured", http.StatusServiceUnavailable)
		return
	}
	dir := s.workspaceDir()
	files, err := listWorkspaceFiles(dir)
	if err != nil {
		log.Println("list workspace:", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	if len(files) == 0 {
		http.Error(w, "no staged files", http.StatusBadRequest)
		return
	}
	if !s.acquire(lockAnalyze) {
		workspaceBusy(w)
		return
	}
	// The budget scales with the batch size; staging is locked for the whole
	// run, so the file count cannot change underneath the loop.
	rounds, toolCalls := analyzeBudget(len(files))
	ag := &analyzeAgent{
		cfg:          s.Config.LLM,
		dir:          dir,
		http:         &http.Client{Timeout: analyzeHTTPTimeout},
		maxRounds:    rounds,
		maxToolCalls: toolCalls,
		onProgress: func(p jobProgress) {
			p.Kind = lockAnalyze
			s.emitProgress(p, false)
		},
		onState: func() {
			s.emitState()
		},
		onFiles: func() {
			s.emitFiles()
		},
	}
	s.emitProgress(jobProgress{Kind: lockAnalyze, Message: "starting", Total: rounds}, false)
	go func() {
		defer s.release(lockAnalyze)
		title, err := ag.run(context.Background(), files)
		if err != nil {
			log.Println("analyze:", err)
			s.emitFail(jobProgress{Kind: lockAnalyze, Error: "analyze failed"})
			return
		}
		// A successful run marks the staged batch as analyzed; adding or
		// deleting staged files clears the flag again (see upload.go).
		st := loadWorkspaceState(dir)
		st.Analyzed = true
		if err := saveWorkspaceState(dir, st); err != nil {
			log.Println("save workspace state:", err)
		}
		s.emitState()
		s.emitDone(jobProgress{Kind: lockAnalyze, Title: title, Time: ag.when})
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (a *analyzeAgent) progress(p jobProgress) {
	if a.onProgress != nil {
		a.onProgress(p)
	}
}

// setTitle persists the chosen title, keeping the pinned time.
func (a *analyzeAgent) setTitle(title string) error {
	st := loadWorkspaceState(a.dir)
	st.Title = title
	if err := saveWorkspaceState(a.dir, st); err != nil {
		return err
	}
	a.title = title
	if a.onState != nil {
		a.onState()
	}
	return nil
}

// textPeek returns a one-line peek at a staged file's leading text, or "" for
// binaries and unreadable files. It never converts; the read tools remain for
// anything the peek cannot cover.
func textPeek(dir, name string) string {
	name, err := sanitizeWorkspaceName(name)
	if err != nil {
		return ""
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, analyzePeekMaxBytes+1))
	if err != nil || len(data) == 0 || bytes.IndexByte(data, 0) >= 0 {
		return ""
	}
	if len(data) > analyzePeekMaxBytes {
		data = data[:analyzePeekMaxBytes]
	}
	// A peek cut mid-rune is not valid UTF-8; trim back to a valid prefix.
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return strings.Join(strings.Fields(string(data)), " ")
}

func (a *analyzeAgent) run(ctx context.Context, files []workspaceFile) (string, error) {
	var b strings.Builder
	b.WriteString("Files staged for upload:\n")
	for _, f := range files {
		fmt.Fprintf(&b, "- `%s` (%s)\n", f.Name, f.Size)
		if peek := textPeek(a.dir, f.Name); peek != "" {
			fmt.Fprintf(&b, "  peek: %s\n", peek)
		}
	}
	messages := []chatMessage{
		{Role: "system", Content: analyzeSystemPrompt},
		{Role: "user", Content: b.String()},
	}
	toolCalls := 0
	for round := 0; round < a.maxRounds; round++ {
		a.progress(jobProgress{
			Message: fmt.Sprintf("round %d/%d", round+1, a.maxRounds),
			Done:    round,
			Total:   a.maxRounds,
		})
		msg, err := a.chatMessage(ctx, &messages, analyzeTools)
		if err != nil {
			return "", err
		}
		// Replies first, image-carrying user messages after them: the tool
		// replies must stay consecutive right behind the assistant message.
		var extra []chatMessage
		for _, tc := range msg.ToolCalls {
			toolCalls++
			reply, userMsgs := a.runTool(ctx, tc)
			messages = append(messages, reply)
			extra = append(extra, userMsgs...)
		}
		messages = append(messages, extra...)
		if a.title != "" {
			return a.title, nil
		}
		if len(msg.ToolCalls) == 0 || toolCalls >= a.maxToolCalls {
			break
		}
	}
	// The read budget is spent: force a decision instead of giving up.
	return a.finalize(ctx, messages)
}

// finalize closes file reading and asks for an immediate decision, so a large
// batch cannot exhaust the round budget without a title. A plain-text answer
// is accepted as a last resort.
func (a *analyzeAgent) finalize(ctx context.Context, messages []chatMessage) (string, error) {
	a.readsClosed = true
	messages = append(messages, chatMessage{
		Role:    "user",
		Content: "Stop reading files and decide now with what you have: call set_title with your best title (optionally set_datetime first).",
	})
	a.progress(jobProgress{Message: "deciding"})
	msg, err := a.chatMessage(ctx, &messages, analyzeDecisionTools)
	if err != nil {
		return "", err
	}
	var extra []chatMessage
	for _, tc := range msg.ToolCalls {
		reply, userMsgs := a.runTool(ctx, tc)
		messages = append(messages, reply)
		extra = append(extra, userMsgs...)
	}
	messages = append(messages, extra...)
	if a.title != "" {
		return a.title, nil
	}
	// Last resort: the model answered in plain text instead of set_title.
	content := strings.TrimSpace(msg.Content)
	if i := strings.IndexAny(content, "\r\n"); i >= 0 {
		content = strings.TrimSpace(content[:i])
	}
	if title, err := sanitizePushTitle(content); err == nil {
		if err := a.setTitle(title); err != nil {
			return "", err
		}
		return title, nil
	}
	return "", errors.New("llm did not set a title")
}

// chatMessage performs one chat-completions round: it appends the assistant
// message to messages and returns it.
func (a *analyzeAgent) chatMessage(ctx context.Context, messages *[]chatMessage, tools []chatTool) (chatReplyMessage, error) {
	resp, err := a.chat(ctx, *messages, tools)
	if err != nil {
		return chatReplyMessage{}, err
	}
	if resp.Error != nil {
		return chatReplyMessage{}, errors.New(resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return chatReplyMessage{}, errors.New("llm: empty response")
	}
	msg := resp.Choices[0].Message
	*messages = append(*messages, chatMessage{Role: "assistant", Content: msg.Content, ToolCalls: msg.ToolCalls})
	return msg, nil
}

// runTool executes one tool call and returns the tool reply plus any extra
// user messages (the image payload for read_file_as_image). Callers must
// append all replies of one assistant turn before the extras: strict
// OpenAI-compatible APIs reject an assistant message whose tool_calls are not
// immediately followed by consecutive tool replies, so interleaving user
// image messages between them breaks the request.
func (a *analyzeAgent) runTool(ctx context.Context, tc chatToolCall) (chatMessage, []chatMessage) {
	reply := func(text string) (chatMessage, []chatMessage) {
		return chatMessage{Role: "tool", ToolCallID: tc.ID, Content: text}, nil
	}
	var args struct {
		Name    string `json:"name"`
		NewName string `json:"new_name"`
		Title   string `json:"title"`
		Time    string `json:"time"`
	}
	if err := unmarshalToolArgs(tc.Function.Arguments, &args); err != nil {
		return reply("invalid arguments: " + err.Error())
	}
	a.progress(jobProgress{Message: tc.Function.Name, File: args.Name})
	if a.readsClosed && (tc.Function.Name == "read_file_as_text" || tc.Function.Name == "read_file_as_image") {
		return reply("error: file reading is closed; call set_title now")
	}
	switch tc.Function.Name {
	case "read_file_as_text":
		text, err := readWorkspaceText(ctx, a.dir, args.Name)
		if err != nil {
			return reply("error: " + err.Error())
		}
		return reply(text)
	case "read_file_as_image":
		mime, data, err := readWorkspaceImage(ctx, a.dir, args.Name)
		if err != nil {
			return reply("error: " + err.Error())
		}
		return chatMessage{Role: "tool", ToolCallID: tc.ID, Content: fmt.Sprintf("image loaded: `%s` (%s, %d bytes)", args.Name, mime, len(data))},
			[]chatMessage{{Role: "user", Content: []chatPart{{
				Type:     "image_url",
				ImageURL: &chatImageURL{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)},
			}}}}
	case "rename_file":
		if err := renameWorkspaceFile(a.dir, args.Name, args.NewName); err != nil {
			return reply("error: " + err.Error())
		}
		if a.onFiles != nil {
			a.onFiles()
		}
		// Echo the new name so later read calls use it; the file list in the
		// initial user message still shows the old one.
		newName, _ := sanitizeWorkspaceName(args.NewName)
		return reply("renamed to `" + newName + "`")
	case "set_title":
		title := strings.TrimSpace(args.Title)
		if title == "" {
			return reply("error: empty title")
		}
		if err := a.setTitle(title); err != nil {
			return reply("error: " + err.Error())
		}
		return reply("title set")
	case "set_datetime":
		when, err := parseAnalyzeTime(args.Time)
		if err != nil {
			return reply("error: invalid time")
		}
		st := loadWorkspaceState(a.dir)
		st.Time = when
		if err := saveWorkspaceState(a.dir, st); err != nil {
			return reply("error: " + err.Error())
		}
		a.when = when
		if a.onState != nil {
			a.onState()
		}
		return reply("datetime set")
	default:
		return reply("unknown tool: " + tc.Function.Name)
	}
}

func (a *analyzeAgent) chat(ctx context.Context, messages []chatMessage, tools []chatTool) (*chatResponse, error) {
	body, err := json.Marshal(chatRequest{
		Model:           a.cfg.Model,
		Messages:        messages,
		Tools:           tools,
		ReasoningEffort: a.cfg.Effort,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range a.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out chatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("llm: bad response: %w", err)
	}
	return &out, nil
}

// unmarshalToolArgs accepts both a JSON object and a JSON string containing
// an object (OpenAI-compatible APIs disagree on tool-call argument encoding).
func unmarshalToolArgs(raw json.RawMessage, dest any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return errors.New("empty")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		raw = bytes.TrimSpace([]byte(s))
		if len(raw) == 0 {
			return errors.New("empty")
		}
	}
	return json.Unmarshal(raw, dest)
}

func parseAnalyzeTime(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("invalid time")
	}
	if t, err := time.Parse(pushTimeLayout, s); err == nil {
		return t.Format(pushTimeLayout), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Format(pushTimeLayout), nil
	}
	return "", errors.New("invalid time")
}

// readWorkspaceText reads a staged file as text: NUL-sniffed, capped at 64 KiB.
// Office/PDF binaries are converted (content-hash cached under .filestor/cache)
// and never mutate staging.
func readWorkspaceText(ctx context.Context, dir, name string) (string, error) {
	name, err := sanitizeWorkspaceName(name)
	if err != nil {
		return "", err
	}
	ext := strings.ToLower(filepath.Ext(name))
	if imageAsTextExts[ext] {
		return "", errUseImageTool
	}
	src := filepath.Join(dir, name)
	if forceTextConvertExts[ext] {
		return convertFileToTextCached(ctx, dir, src)
	}
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, analyzeTextMaxBytes+1))
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return convertFileToTextCached(ctx, dir, src)
	}
	return capConvertedText(data), nil
}

// readWorkspaceImage loads a staged file as jpeg/png/gif for the model.
// Small native jpeg/png/gif are returned as-is; anything else is converted
// (content-hash cached under .filestor/cache).
func readWorkspaceImage(ctx context.Context, dir, name string) (string, []byte, error) {
	name, err := sanitizeWorkspaceName(name)
	if err != nil {
		return "", nil, err
	}
	src := filepath.Join(dir, name)
	f, err := os.Open(src)
	if err != nil {
		return "", nil, err
	}
	head := make([]byte, 16)
	n, _ := f.Read(head)
	mime := sniffImageMIME(head[:n])
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return "", nil, err
	}
	if mime != "" && info.Size() <= analyzeImageMaxBytes {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			_ = f.Close()
			return "", nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, analyzeImageMaxBytes+1))
		_ = f.Close()
		if err != nil {
			return "", nil, err
		}
		if int64(len(data)) <= analyzeImageMaxBytes {
			return mime, data, nil
		}
	} else {
		_ = f.Close()
	}
	return convertFileToLLMImageCached(ctx, dir, src)
}
