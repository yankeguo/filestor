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
	// sniffMaxBytes is the leading window NUL-sniffed to tell staged text
	// files from unknown binaries.
	sniffMaxBytes = 8 << 10
	// readFileMaxBytes caps how much of a staging text file is paged from
	// memory by read_file; larger files are truncated and the reply says so.
	readFileMaxBytes = 8 << 20
	// read_file pages: offset is 1-based, limit defaults to
	// readFileDefaultLimit and is capped at readFileMaxLimit.
	readFileDefaultLimit = 200
	readFileMaxLimit     = 500
	// analyzeRunTimeout caps one whole analyze run; the per-request HTTP
	// timeout alone cannot stop a long tool loop.
	analyzeRunTimeout = 30 * time.Minute
	// analyzeMaxImages caps the base64 image user messages kept in the
	// message history (each is ~11MB); older ones are replaced by a
	// placeholder.
	analyzeMaxImages = 4
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
- The user message lists each staged file with its size and kind; indented under each source are its converted, model-readable forms: a ⟪name.txt⟫ full text with its line count, ⟪name.pNN.jpg⟫ rendered document pages, ⟪name.fN.jpg⟫ video frames, or ⟪name.jpg⟫ a normalized image. A native text file shows its line count and a one-line "peek" of its leading content instead. Names, peeks, and line counts are often enough — read only when you need more.
- read_file(name, offset, limit) reads text in pages of lines: offset is the 1-based start line (default 1), limit the page size (default 200, at most 500; the reply itself is capped at 64 KiB), and the reply header reports "lines X-Y of N" so you can page on with the next offset. It accepts a staged text file or a derived ⟪name.txt⟫; calling it on a document, image, or video source name is an error — use that file's derived forms instead.
- load_media(name) loads images for you to see: pass a staged image, document, or video name to load all its derived images (normalized image, rendered pages, extracted frames), or a single derived image name like ⟪report.docx.p01.jpg⟫.
- When a document's text form is missing, empty, or too short to judge — a scanned or all-image document — load_media its page images instead.
- Skip very large files (e.g. a video or PDF of several hundred MB): judge them by name instead; a huge file must not block your decision.
- The title is a short phrase (at most 40 characters) in the same language as the content, e.g. "weekly-report" or "月度账单".
- If the contents contain a clear document date or datetime, call set_datetime with it (YYYY-MM-DD or YYYY-MM-DDTHH:mm), before or in the same turn as set_title. Do not guess.
- If a staged file's name is clearly messy or uninformative — camera/scanner codes like ⟪IMG_2048.jpg⟫ or ⟪SCAN_0001.pdf⟫, timestamp-only screenshot names, random hashes, placeholder names like ⟪untitled⟫ or ⟪新建文档⟫, noise like ⟪final2⟫, ⟪copy of⟫, ⟪(1)⟫, or a name date that is redundant or contradicts the document's actual date — and you are confident about its content, call rename_file with a short, descriptive new name in the same language as the content. Use the document's actual date in the new name or drop the date entirely; never keep a date you know is wrong. Keep the extension; use only letters, digits, dash, underscore, dot; rename each file at most once; never pick a name another staged file already has. When in doubt, keep the original name — renaming is optional and must not delay set_title. Derived forms keep the original staged file name.
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
				"name": map[string]any{"type": "string", "description": "File name, without backticks"},
			},
			"required": []string{"name"},
		},
	}}
}

var analyzeTools = []chatTool{
	{Type: "function", Function: chatToolFunction{
		Name: "read_file",
		Description: `Read one page of lines from a staged text file or a derived "name.txt" text form; ` +
			`the reply header reports "lines X-Y of N" so you can keep paging with offset.`,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":   map[string]any{"type": "string", "description": "Staged text file name or derived text form, without backticks"},
				"offset": map[string]any{"type": "integer", "description": "1-based start line (default 1)"},
				"limit":  map[string]any{"type": "integer", "description": "Lines to read (default 200, max 500)"},
			},
			"required": []string{"name"},
		},
	}},
	nameParamTool("load_media", "Load images to view: pass a staged image, document, or video name to load all its derived images (normalized image, rendered pages, extracted frames), or a single derived image name."),
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
	cfg          ChatConfig
	dir          string
	state        *workspaceStateStore
	http         *http.Client
	maxRounds    int
	maxToolCalls int
	title        string
	when         string
	readsClosed  bool
	// entries maps the current staged name to its pre-converted forms;
	// derived maps every product name in .filestor/analyze back to its entry.
	entries    map[string]*analyzeEntry
	derived    map[string]*analyzeEntry
	onProgress func(jobProgress)
	onState    func()
	onFiles    func()
}

func (s *Server) handleUploadAnalyze(w http.ResponseWriter, r *http.Request) {
	if s.Config == nil || s.Config.LLM.Chat.URL == "" {
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
		cfg:          s.Config.LLM.Chat,
		dir:          dir,
		state:        s.state,
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
		defer func() {
			if r := recover(); r != nil {
				log.Println("analyze panic:", r)
				s.emitFail(jobProgress{Kind: lockAnalyze, Error: "internal error"})
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), analyzeRunTimeout)
		defer cancel()
		title, err := ag.run(ctx, files)
		if err != nil {
			log.Println("analyze:", err)
			s.emitFail(jobProgress{Kind: lockAnalyze, Error: "analyze failed"})
			return
		}
		// A successful run marks the staged batch as analyzed; adding or
		// deleting staged files clears the flag again (see upload.go).
		st := s.state.get()
		st.Analyzed = true
		if err := s.state.save(st); err != nil {
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
	st := a.state.get()
	st.Title = title
	if err := a.state.save(st); err != nil {
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

// analyzeKind buckets a staged file for pre-conversion.
type analyzeKind int

const (
	kindText analyzeKind = iota
	kindDocument
	kindImage
	kindVideo
	kindOther
)

func (k analyzeKind) String() string {
	switch k {
	case kindDocument:
		return "document"
	case kindImage:
		return "image"
	case kindVideo:
		return "video"
	case kindOther:
		return "binary"
	default:
		return "text"
	}
}

// classifyStagedFile buckets a staged file. Extensions win over content
// sniffing (an .svg is text but belongs to the images); everything else is
// text unless its first 8 KiB contain a NUL.
func classifyStagedFile(dir, name string) analyzeKind {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case documentExts[ext]:
		return kindDocument
	case imageExts[ext]:
		return kindImage
	case videoExts[ext]:
		return kindVideo
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return kindOther
	}
	defer f.Close()
	buf := make([]byte, sniffMaxBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return kindOther
	}
	if bytes.IndexByte(buf[:n], 0) >= 0 {
		return kindOther
	}
	return kindText
}

// analyzeEntry is one staged file plus its converted, model-readable forms,
// linked into .filestor/analyze under names derived from the staged name.
type analyzeEntry struct {
	Name      string // current staged name (rename_file re-keys it)
	Size      string // human size from the staging list
	Kind      analyzeKind
	Lines     int      // native text: lines in the readable prefix
	Truncated bool     // native text: larger than readFileMaxBytes
	Peek      string   // native text: one-line peek at the leading content
	Text      string   // derived full-text form `name.txt`
	TextLines int      // lines in the derived text form
	Pages     []string // derived rendered document pages `name.pNN.jpg`
	Frames    []string // derived video frames `name.fN.jpg`
	Image     string   // derived normalized image `name.jpg` ("" = native)
	Failed    bool     // every conversion of a convertible kind failed
}

// derivedNames lists the entry's product names under .filestor/analyze.
func (e *analyzeEntry) derivedNames() []string {
	var out []string
	if e.Text != "" {
		out = append(out, e.Text)
	}
	out = append(out, e.Pages...)
	out = append(out, e.Frames...)
	if e.Image != "" {
		out = append(out, e.Image)
	}
	return out
}

// countTextLines counts lines the way read_file pages them: a trailing
// newline does not start a new line.
func countTextLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

func countFileLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return countTextLines(data)
}

// textFileStats returns the line count of a staged text file's readable
// prefix and whether the file exceeds readFileMaxBytes.
func textFileStats(dir, name string) (lines int, truncated bool) {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return 0, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, readFileMaxBytes+1))
	if err != nil {
		return 0, false
	}
	truncated = len(data) > readFileMaxBytes
	if truncated {
		data = data[:readFileMaxBytes]
	}
	return countTextLines(data), truncated
}

// linkDerived places one conversion product under .filestor/analyze as a hard
// link to the cache file, falling back to a copy where links are unsupported.
func linkDerived(analyzeDir, srcPath, name string) bool {
	dst := filepath.Join(analyzeDir, name)
	if err := os.Link(srcPath, dst); err == nil {
		return true
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		log.Println("link analyze product:", err)
		return false
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		log.Println("link analyze product:", err)
		return false
	}
	return true
}

// prepEntry classifies one staged file and pre-converts it, best effort,
// toward the two model-readable forms: paged text and normalized images. A
// failed conversion never aborts the run; the entry is marked instead.
func prepEntry(ctx context.Context, dir, analyzeDir string, f workspaceFile) *analyzeEntry {
	e := &analyzeEntry{Name: f.Name, Size: f.Size, Kind: classifyStagedFile(dir, f.Name)}
	src := filepath.Join(dir, f.Name)
	linkText := func(p string) {
		name := f.Name + ".txt"
		if linkDerived(analyzeDir, p, name) {
			e.Text = name
			e.TextLines = countFileLines(p)
		}
	}
	switch e.Kind {
	case kindText:
		e.Lines, e.Truncated = textFileStats(dir, f.Name)
		e.Peek = textPeek(dir, f.Name)
	case kindDocument:
		ctx, cancel := withConvertTimeout(ctx, convertTimeoutLong)
		defer cancel()
		if p, err := convertToTextFile(ctx, dir, src); err == nil {
			linkText(p)
		}
		if pages, err := convertToPageFiles(ctx, dir, src); err == nil {
			for i, p := range pages {
				name := fmt.Sprintf("%s.p%02d.jpg", f.Name, i+1)
				if linkDerived(analyzeDir, p, name) {
					e.Pages = append(e.Pages, name)
				}
			}
		}
		e.Failed = e.Text == "" && len(e.Pages) == 0
	case kindImage:
		if nativeImageMIME(src) != "" {
			break // native jpeg/png/gif within the cap loads straight from staging
		}
		ctx, cancel := withConvertTimeout(ctx, convertTimeout)
		defer cancel()
		if p, err := convertToImageFile(ctx, dir, src); err == nil {
			name := f.Name + ".jpg"
			if linkDerived(analyzeDir, p, name) {
				e.Image = name
			}
		}
		e.Failed = e.Image == ""
	case kindVideo:
		ctx, cancel := withConvertTimeout(ctx, convertTimeoutLong)
		defer cancel()
		if frames, err := convertToFrameFiles(ctx, dir, src); err == nil {
			for i, p := range frames {
				name := fmt.Sprintf("%s.f%d.jpg", f.Name, i+1)
				if linkDerived(analyzeDir, p, name) {
					e.Frames = append(e.Frames, name)
				}
			}
		}
		e.Failed = len(e.Frames) == 0
	case kindOther:
		// Best effort only, and only for files small enough to be worth a
		// soffice/pandoc attempt; larger unknowns are judged by name.
		if info, err := os.Stat(src); err == nil && info.Size() <= otherConvertMaxBytes {
			ctx, cancel := withConvertTimeout(ctx, convertTimeoutLong)
			defer cancel()
			if p, err := convertToTextFile(ctx, dir, src); err == nil {
				linkText(p)
			}
		}
	}
	return e
}

// prepAnalyze rebuilds .filestor/analyze from scratch and pre-converts every
// staged file, one progress event per file.
func prepAnalyze(ctx context.Context, dir string, files []workspaceFile, progress func(jobProgress)) []*analyzeEntry {
	if err := resetAnalyzeDir(dir); err != nil {
		log.Println("reset analyze dir:", err)
	}
	analyzeDir := workspaceAnalyzePath(dir)
	entries := make([]*analyzeEntry, 0, len(files))
	for i, f := range files {
		if progress != nil {
			progress(jobProgress{Message: "converting", File: f.Name, Done: i, Total: len(files)})
		}
		entries = append(entries, prepEntry(ctx, dir, analyzeDir, f))
	}
	return entries
}

func quoteNames(names []string) string {
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('`')
		b.WriteString(n)
		b.WriteByte('`')
	}
	return b.String()
}

// buildAnalyzeListing renders the initial user message: one line per staged
// file with its converted forms indented below, so the model can plan which
// form to read.
func buildAnalyzeListing(entries []*analyzeEntry) string {
	var b strings.Builder
	b.WriteString("Files staged for upload (converted forms listed under each source):\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "- `%s` (%s, %s", e.Name, e.Size, e.Kind)
		if e.Kind == kindText {
			fmt.Fprintf(&b, ", %d lines", e.Lines)
			if e.Truncated {
				fmt.Fprintf(&b, ", only the first %s are readable", formatSize(readFileMaxBytes))
			}
		}
		b.WriteString(")")
		switch e.Kind {
		case kindText:
			b.WriteString("\n")
			if e.Peek != "" {
				fmt.Fprintf(&b, "  peek: %s\n", e.Peek)
			}
		default:
			if e.Failed {
				b.WriteString(" — conversion failed")
			}
			if e.Kind == kindOther && e.Text == "" {
				b.WriteString(" — no readable form")
			}
			b.WriteString("\n")
			if e.Text != "" {
				fmt.Fprintf(&b, "  text: `%s` (%d lines)\n", e.Text, e.TextLines)
			}
			if len(e.Pages) > 0 {
				fmt.Fprintf(&b, "  pages: %s\n", quoteNames(e.Pages))
			}
			if len(e.Frames) > 0 {
				fmt.Fprintf(&b, "  frames: %s\n", quoteNames(e.Frames))
			}
			if e.Image != "" {
				fmt.Fprintf(&b, "  image: `%s`\n", e.Image)
			}
		}
	}
	return b.String()
}

// indexEntries builds the agent's name lookups from a prep manifest.
func (a *analyzeAgent) indexEntries(list []*analyzeEntry) {
	a.entries = make(map[string]*analyzeEntry, len(list))
	a.derived = make(map[string]*analyzeEntry)
	for _, e := range list {
		a.entries[e.Name] = e
		for _, n := range e.derivedNames() {
			a.derived[n] = e
		}
	}
}

func (a *analyzeAgent) run(ctx context.Context, files []workspaceFile) (string, error) {
	list := prepAnalyze(ctx, a.dir, files, a.progress)
	a.indexEntries(list)
	messages := []chatMessage{
		{Role: "system", Content: analyzeSystemPrompt},
		{Role: "user", Content: buildAnalyzeListing(list)},
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
		trimImageMessages(messages)
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

// resolveTextName maps a read_file name to a path: a staged text file reads
// straight from staging, a derived `name.txt` reads from .filestor/analyze,
// and anything else errors with a pointer at the right form or tool.
func (a *analyzeAgent) resolveTextName(name string) (string, error) {
	if e := a.entries[name]; e != nil {
		if e.Kind == kindText {
			return filepath.Join(a.dir, e.Name), nil
		}
		if e.Text != "" {
			return "", fmt.Errorf("`%s` is a %s; read its text form `%s` instead (or view it with load_media)", name, e.Kind, e.Text)
		}
		return "", fmt.Errorf("`%s` is a %s with no text form; view it with load_media", name, e.Kind)
	}
	if _, ok := a.derived[name]; ok {
		if strings.HasSuffix(name, ".txt") {
			return filepath.Join(workspaceAnalyzePath(a.dir), name), nil
		}
		return "", fmt.Errorf("`%s` is an image; load it with load_media", name)
	}
	return "", fmt.Errorf("no such file: `%s`", name)
}

// toolReadFile answers one read_file call; errors come back as reply text so
// the model can correct itself.
func (a *analyzeAgent) toolReadFile(name string, offset, limit int) string {
	name, err := sanitizeWorkspaceName(name)
	if err != nil {
		return "error: " + err.Error()
	}
	path, err := a.resolveTextName(name)
	if err != nil {
		return "error: " + err.Error()
	}
	return pagedText(path, name, offset, limit)
}

// pagedText reads one page of lines from a text file: offset is the 1-based
// start line, limit the page size. The reply header reports "lines X-Y of N"
// so the model can keep paging; the body never exceeds analyzeTextMaxBytes,
// and only the first readFileMaxBytes of the file are readable at all.
func pagedText(path, name string, offset, limit int) string {
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = readFileDefaultLimit
	}
	if limit > readFileMaxLimit {
		limit = readFileMaxLimit
	}
	f, err := os.Open(path)
	if err != nil {
		return "error: " + err.Error()
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, readFileMaxBytes+1))
	if err != nil {
		return "error: " + err.Error()
	}
	truncated := len(data) > readFileMaxBytes
	if truncated {
		data = data[:readFileMaxBytes]
	}
	lines := strings.Split(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // a trailing newline does not start a new line
	}
	total := len(lines)
	if total == 0 {
		return fmt.Sprintf("`%s` is empty (0 lines)", name)
	}
	if offset > total {
		return fmt.Sprintf("`%s` has %d lines; offset %d is past the end", name, total, offset)
	}
	end := min(offset+limit-1, total)
	sel := lines[offset-1 : end]
	body := strings.Join(sel, "\n")
	var notes []string
	if len(body) > analyzeTextMaxBytes {
		// Drop whole trailing lines until the page fits the reply cap.
		for len(sel) > 1 && len(body) > analyzeTextMaxBytes {
			sel = sel[:len(sel)-1]
			body = strings.Join(sel, "\n")
		}
		if len(body) > analyzeTextMaxBytes {
			// A single line over the cap: cut it back to valid UTF-8.
			body = body[:analyzeTextMaxBytes]
			for len(body) > 0 && !utf8.ValidString(body) {
				body = body[:len(body)-1]
			}
		}
		end = offset + len(sel) - 1
		notes = append(notes, fmt.Sprintf("reply capped at %s", formatSize(analyzeTextMaxBytes)))
	}
	if truncated {
		notes = append(notes, fmt.Sprintf("only the first %s of the file are readable", formatSize(readFileMaxBytes)))
	}
	header := fmt.Sprintf("`%s`: lines %d-%d of %d", name, offset, end, total)
	if len(notes) > 0 {
		header += " (" + strings.Join(notes, "; ") + ")"
	}
	return header + "\n" + body
}

// loadedImage is one image ready to be sent to the model.
type loadedImage struct {
	name string
	mime string
	data []byte
}

// loadImageFile reads one image for the model, enforcing the size cap and
// sniffing the real mime (a soffice fallback page may be png).
func loadImageFile(path, name string) (loadedImage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return loadedImage{}, err
	}
	if len(data) == 0 || int64(len(data)) > analyzeImageMaxBytes {
		return loadedImage{}, fmt.Errorf("`%s` exceeds %s", name, formatSize(analyzeImageMaxBytes))
	}
	mime := sniffImageMIME(data)
	if mime == "" {
		mime = "image/jpeg"
	}
	return loadedImage{name: name, mime: mime, data: data}, nil
}

// toolLoadMedia answers one load_media call: a staged image/document/video
// name loads all of its derived images (normalized image, rendered pages,
// extracted frames; a small native jpeg/png/gif loads straight from staging),
// a derived image name loads just that one. Errors come back as reply text.
func (a *analyzeAgent) toolLoadMedia(name string) (string, []loadedImage) {
	name, err := sanitizeWorkspaceName(name)
	if err != nil {
		return "error: " + err.Error(), nil
	}
	analyzeDir := workspaceAnalyzePath(a.dir)
	var names, paths []string
	var label string
	derived := func(list []string) {
		for _, n := range list {
			names = append(names, n)
			paths = append(paths, filepath.Join(analyzeDir, n))
		}
	}
	if e := a.entries[name]; e != nil {
		switch e.Kind {
		case kindText:
			return fmt.Sprintf("error: `%s` is text; read it with read_file", name), nil
		case kindImage:
			if e.Image == "" {
				names, paths = []string{e.Name}, []string{filepath.Join(a.dir, e.Name)}
				label = fmt.Sprintf("loaded `%s`", e.Name)
			} else {
				derived([]string{e.Image})
				label = fmt.Sprintf("loaded `%s`", e.Image)
			}
		case kindDocument:
			if len(e.Pages) == 0 {
				return fmt.Sprintf("error: `%s` has no page images", name), nil
			}
			derived(e.Pages)
			label = fmt.Sprintf("loaded %d page(s) of `%s`", len(e.Pages), e.Name)
		case kindVideo:
			if len(e.Frames) == 0 {
				return fmt.Sprintf("error: `%s` has no frame images", name), nil
			}
			derived(e.Frames)
			label = fmt.Sprintf("loaded %d frame(s) of `%s`", len(e.Frames), e.Name)
		default:
			return fmt.Sprintf("error: `%s` has no image form", name), nil
		}
	} else if _, ok := a.derived[name]; ok {
		if strings.HasSuffix(name, ".txt") {
			return fmt.Sprintf("error: `%s` is text; read it with read_file", name), nil
		}
		derived([]string{name})
		label = fmt.Sprintf("loaded `%s`", name)
	} else {
		return fmt.Sprintf("error: no such file: `%s`", name), nil
	}
	var imgs []loadedImage
	var skipped []string
	for i, p := range paths {
		img, err := loadImageFile(p, names[i])
		if err != nil {
			skipped = append(skipped, names[i])
			continue
		}
		imgs = append(imgs, img)
	}
	if len(imgs) == 0 {
		return fmt.Sprintf("error: could not load `%s`", name), nil
	}
	if len(skipped) > 0 {
		label += "; skipped " + strings.Join(skipped, ", ")
	}
	return label, imgs
}

// runTool executes one tool call and returns the tool reply plus any extra
// user messages (the image payload for load_media). Callers must append all
// replies of one assistant turn before the extras: strict OpenAI-compatible
// APIs reject an assistant message whose tool_calls are not immediately
// followed by consecutive tool replies, so interleaving user image messages
// between them breaks the request.
func (a *analyzeAgent) runTool(ctx context.Context, tc chatToolCall) (chatMessage, []chatMessage) {
	reply := func(text string) (chatMessage, []chatMessage) {
		return chatMessage{Role: "tool", ToolCallID: tc.ID, Content: text}, nil
	}
	var args struct {
		Name    string `json:"name"`
		NewName string `json:"new_name"`
		Title   string `json:"title"`
		Time    string `json:"time"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
	}
	if err := unmarshalToolArgs(tc.Function.Arguments, &args); err != nil {
		return reply("invalid arguments: " + err.Error())
	}
	a.progress(jobProgress{Message: tc.Function.Name, File: args.Name})
	if a.readsClosed && (tc.Function.Name == "read_file" || tc.Function.Name == "load_media") {
		return reply("error: file reading is closed; call set_title now")
	}
	switch tc.Function.Name {
	case "read_file":
		return reply(a.toolReadFile(args.Name, args.Offset, args.Limit))
	case "load_media":
		label, imgs := a.toolLoadMedia(args.Name)
		if len(imgs) == 0 {
			return reply(label) // label carries the error text
		}
		parts := make([]chatPart, 0, len(imgs))
		for _, img := range imgs {
			parts = append(parts, chatPart{
				Type:     "image_url",
				ImageURL: &chatImageURL{URL: "data:" + img.mime + ";base64," + base64.StdEncoding.EncodeToString(img.data)},
			})
		}
		return chatMessage{Role: "tool", ToolCallID: tc.ID, Content: label},
			[]chatMessage{{Role: "user", Content: parts}}
	case "rename_file":
		if err := renameWorkspaceFile(a.dir, args.Name, args.NewName); err != nil {
			return reply("error: " + err.Error())
		}
		if a.onFiles != nil {
			a.onFiles()
		}
		// Echo the new name so later read calls use it; the file list in the
		// initial user message still shows the old one.
		oldName, _ := sanitizeWorkspaceName(args.Name)
		newName, _ := sanitizeWorkspaceName(args.NewName)
		if e := a.entries[oldName]; e != nil {
			delete(a.entries, oldName)
			e.Name = newName
			a.entries[newName] = e
		}
		return reply("renamed to `" + newName + "`")
	case "set_title":
		// Apply the same sanitizing as the finalize plain-text fallback so
		// the stored title always fits the push rules.
		title, err := sanitizePushTitle(args.Title)
		if err != nil {
			return reply("error: title is empty after sanitizing (letters, digits, '_' and '.' only, everything else folds to '-'); call set_title again with a descriptive title")
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
		st := a.state.get()
		st.Time = when
		if err := a.state.save(st); err != nil {
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

// trimImageMessages replaces all but the newest analyzeMaxImages
// image-carrying user messages with a plain-text placeholder: a base64 image
// is ~11MB and would otherwise accumulate in the history for the whole run.
func trimImageMessages(messages []chatMessage) {
	seen := 0
	for i := len(messages) - 1; i >= 0; i-- {
		m := &messages[i]
		parts, ok := m.Content.([]chatPart)
		if !ok || m.Role != "user" {
			continue
		}
		isImage := false
		for _, p := range parts {
			if p.ImageURL != nil {
				isImage = true
				break
			}
		}
		if !isImage {
			continue
		}
		seen++
		if seen > analyzeMaxImages {
			m.Content = "[image omitted]"
		}
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
		// Keep the upstream error body short: it lands in logs (the SSE
		// error event is always the constant "analyze failed").
		const maxErrBody = 256
		msg := strings.TrimSpace(string(data))
		if len(msg) > maxErrBody {
			msg = msg[:maxErrBody] + "..."
		}
		return nil, fmt.Errorf("llm: HTTP %d: %s", resp.StatusCode, msg)
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
