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
)

const (
	suggestMaxRounds     = 8
	suggestTextMaxBytes  = 64 << 10
	suggestImageMaxBytes = 8 << 20
	suggestHTTPTimeout   = 120 * time.Second
)

const suggestSystemPrompt = `You invent a short, descriptive title for a batch of staged files that will be uploaded to object storage under "YYYYMMDDhhmm-TITLE/".
- Inspect file contents with the tools when the file names alone are not enough. read_file_as_text and read_file_as_image convert office documents, PDFs, and other formats automatically; you do not need to care about the conversion.
- The title should be a short phrase (at most 40 characters) in the same language as the content, e.g. "weekly-report" or "月度账单".
- When you have decided, call set_title exactly once with the raw title.`

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

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string         `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"message"`
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
				"name": map[string]any{"type": "string", "description": "File name from the staged file list"},
			},
			"required": []string{"name"},
		},
	}}
}

var suggestTools = []chatTool{
	nameParamTool("read_file_as_text", "Read a staged file as text. Office documents and PDFs are converted automatically."),
	nameParamTool("read_file_as_image", "Load a staged file as an image (jpeg/png/gif). Oversized or other formats are converted automatically."),
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
}

// suggestAgent runs the chat-completions tool loop against the configured
// OpenAI-compatible endpoint.
type suggestAgent struct {
	cfg   LLMConfig
	dir   string
	http  *http.Client
	title string
}

func (s *Server) handleUploadSuggest(w http.ResponseWriter, r *http.Request) {
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
	ag := &suggestAgent{
		cfg:  s.Config.LLM,
		dir:  dir,
		http: &http.Client{Timeout: suggestHTTPTimeout},
	}
	title, err := ag.run(r.Context(), files)
	if err != nil {
		log.Println("suggest:", err)
		http.Error(w, "suggest failed", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "title": title})
}

func (a *suggestAgent) run(ctx context.Context, files []workspaceFile) (string, error) {
	var b strings.Builder
	b.WriteString("Files staged for upload:\n")
	for _, f := range files {
		fmt.Fprintf(&b, "- %s (%s)\n", f.Name, f.Size)
	}
	messages := []chatMessage{
		{Role: "system", Content: suggestSystemPrompt},
		{Role: "user", Content: b.String()},
	}
	for round := 0; round < suggestMaxRounds; round++ {
		resp, err := a.chat(ctx, messages)
		if err != nil {
			return "", err
		}
		if resp.Error != nil {
			return "", errors.New(resp.Error.Message)
		}
		if len(resp.Choices) == 0 {
			return "", errors.New("llm: empty response")
		}
		msg := resp.Choices[0].Message
		messages = append(messages, chatMessage{Role: "assistant", Content: msg.Content, ToolCalls: msg.ToolCalls})
		for _, tc := range msg.ToolCalls {
			messages = append(messages, a.runTool(ctx, tc)...)
		}
		if a.title != "" {
			return a.title, nil
		}
		if len(msg.ToolCalls) == 0 {
			break
		}
	}
	return "", errors.New("llm did not set a title")
}

// runTool executes one tool call and returns the message(s) to append: always
// a tool reply, plus an extra user message carrying the image for
// read_file_as_image.
func (a *suggestAgent) runTool(ctx context.Context, tc chatToolCall) []chatMessage {
	reply := func(text string) []chatMessage {
		return []chatMessage{{Role: "tool", ToolCallID: tc.ID, Content: text}}
	}
	var args struct {
		Name  string `json:"name"`
		Title string `json:"title"`
	}
	if err := unmarshalToolArgs(tc.Function.Arguments, &args); err != nil {
		return reply("invalid arguments: " + err.Error())
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
		return []chatMessage{
			{Role: "tool", ToolCallID: tc.ID, Content: fmt.Sprintf("image loaded: %s (%s, %d bytes)", args.Name, mime, len(data))},
			{Role: "user", Content: []chatPart{{
				Type:     "image_url",
				ImageURL: &chatImageURL{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)},
			}}},
		}
	case "set_title":
		title := strings.TrimSpace(args.Title)
		if title == "" {
			return reply("error: empty title")
		}
		st := loadWorkspaceState(a.dir)
		st.Title = title
		if err := saveWorkspaceState(a.dir, st); err != nil {
			return reply("error: " + err.Error())
		}
		a.title = title
		return reply("title set")
	default:
		return reply("unknown tool: " + tc.Function.Name)
	}
}

func (a *suggestAgent) chat(ctx context.Context, messages []chatMessage) (*chatResponse, error) {
	body, err := json.Marshal(chatRequest{
		Model:           a.cfg.Model,
		Messages:        messages,
		Tools:           suggestTools,
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

// readWorkspaceText reads a staged file as text: NUL-sniffed, capped at 64 KiB.
// Office/PDF binaries are converted in a temp dir and never mutate staging.
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
		return convertFileToText(ctx, src)
	}
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, suggestTextMaxBytes+1))
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return convertFileToText(ctx, src)
	}
	return capConvertedText(data), nil
}

// readWorkspaceImage loads a staged file as jpeg/png/gif for the model.
// Small native jpeg/png/gif are returned as-is; anything else is converted.
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
	if mime != "" && info.Size() <= suggestImageMaxBytes {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			_ = f.Close()
			return "", nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, suggestImageMaxBytes+1))
		_ = f.Close()
		if err != nil {
			return "", nil, err
		}
		if int64(len(data)) <= suggestImageMaxBytes {
			return mime, data, nil
		}
	} else {
		_ = f.Close()
	}
	return convertFileToLLMImage(ctx, src)
}
