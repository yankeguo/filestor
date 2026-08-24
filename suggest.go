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
- Inspect file contents with the tools when the file names alone are not enough.
- The title should be a short phrase (at most 40 characters) in the same language as the content, e.g. "weekly-report" or "月度账单".
- When you have decided, call set_title exactly once with the raw title.`

var suggestImageMIMEs = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

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
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
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
	nameParamTool("read_text_file", "Read the contents of a staged text file."),
	nameParamTool("read_image_file", "Load a staged image file (png/jpg/jpeg/gif/webp) so you can see it."),
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
		http.Error(w, "suggest failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "title": title})
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
			messages = append(messages, a.runTool(tc)...)
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
// read_image_file.
func (a *suggestAgent) runTool(tc chatToolCall) []chatMessage {
	reply := func(text string) []chatMessage {
		return []chatMessage{{Role: "tool", ToolCallID: tc.ID, Content: text}}
	}
	var args struct {
		Name  string `json:"name"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return reply("invalid arguments: " + err.Error())
	}
	switch tc.Function.Name {
	case "read_text_file":
		text, err := readWorkspaceText(a.dir, args.Name)
		if err != nil {
			return reply("error: " + err.Error())
		}
		return reply(text)
	case "read_image_file":
		mime, data, err := readWorkspaceImage(a.dir, args.Name)
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

// readWorkspaceText reads a staged file as text: NUL-sniffed, capped at 64 KiB.
func readWorkspaceText(dir, name string) (string, error) {
	name, err := sanitizeWorkspaceName(name)
	if err != nil {
		return "", err
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, suggestTextMaxBytes+1))
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", errors.New("not a text file")
	}
	truncated := len(data) > suggestTextMaxBytes
	if truncated {
		data = data[:suggestTextMaxBytes]
	}
	out := string(data)
	if strings.TrimSpace(out) == "" {
		return "(empty file)", nil
	}
	if truncated {
		out += "\n... (truncated)"
	}
	return out, nil
}

// readWorkspaceImage reads a staged image file, capped at 8 MiB.
func readWorkspaceImage(dir, name string) (string, []byte, error) {
	name, err := sanitizeWorkspaceName(name)
	if err != nil {
		return "", nil, err
	}
	mime, ok := suggestImageMIMEs[strings.ToLower(filepath.Ext(name))]
	if !ok {
		return "", nil, errors.New("not a supported image (png/jpg/jpeg/gif/webp)")
	}
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		return "", nil, err
	}
	if info.Size() > suggestImageMaxBytes {
		return "", nil, fmt.Errorf("image too large (%s, max 8 MiB)", formatSize(info.Size()))
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", nil, err
	}
	return mime, data, nil
}
