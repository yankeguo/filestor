package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	lockStage   = "stage"
	lockAnalyze = "analyze"
	lockPush    = "push"

	eventSnapshot = "snapshot"
	eventLock     = "lock"
	eventFiles    = "files"
	eventState    = "state"
	eventProgress = "progress"
	eventDone     = "done"
	eventError    = "error"

	eventsHeartbeat     = 15 * time.Second
	progressMinInterval = 100 * time.Millisecond
	eventsChanSize      = 16
)

// jobProgress is the JSON payload for progress/done/error and the snapshot job.
type jobProgress struct {
	Kind       string `json:"kind,omitempty"`
	Message    string `json:"message,omitempty"`
	File       string `json:"file,omitempty"`
	Done       int    `json:"done,omitempty"`
	Total      int    `json:"total,omitempty"`
	DoneBytes  int64  `json:"done_bytes,omitempty"`
	TotalBytes int64  `json:"total_bytes,omitempty"`
	Title      string `json:"title,omitempty"`
	Time       string `json:"time,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
	Error      string `json:"error,omitempty"`
}

type eventsSnapshot struct {
	Lock  string          `json:"lock"`
	Files []workspaceFile `json:"files"`
	State workspaceState  `json:"state"`
	Job   *jobProgress    `json:"job,omitempty"`
}

type sseEvent struct {
	Name string
	Data any
}

type eventHub struct {
	mu       sync.Mutex
	lock     string
	job      *jobProgress
	lastProg time.Time
	subs     map[chan sseEvent]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[chan sseEvent]struct{})}
}

func (s *Server) acquire(kind string) bool {
	s.hub.mu.Lock()
	if s.hub.lock != "" {
		s.hub.mu.Unlock()
		return false
	}
	s.hub.lock = kind
	// A new operation supersedes the last job's result and its throttle state.
	s.hub.job = nil
	s.hub.lastProg = time.Time{}
	s.hub.mu.Unlock()
	s.hub.broadcast(eventLock, map[string]string{"lock": kind})
	return true
}

func (s *Server) release(kind string) {
	s.hub.mu.Lock()
	if s.hub.lock != kind {
		s.hub.mu.Unlock()
		return
	}
	s.hub.lock = ""
	s.hub.mu.Unlock()
	s.hub.broadcast(eventLock, map[string]string{"lock": ""})
}

func (s *Server) lockKind() string {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	return s.hub.lock
}

func (s *Server) lastJob() jobProgress {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if s.hub.job == nil {
		return jobProgress{}
	}
	return *s.hub.job
}

func (s *Server) busyForState() bool {
	k := s.lockKind()
	return k == lockAnalyze || k == lockPush
}

func (h *eventHub) subscribe() chan sseEvent {
	ch := make(chan sseEvent, eventsChanSize)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *eventHub) unsubscribe(ch chan sseEvent) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// broadcast fans an event out to all subscribers. A subscriber whose buffer
// is full is evicted (channel closed); its handler then ends the response and
// the browser's EventSource reconnects to a fresh snapshot, so slow clients
// self-heal instead of silently missing lock/done/error events.
func (h *eventHub) broadcast(name string, data any) {
	ev := sseEvent{Name: name, Data: data}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

func (s *Server) setJob(p jobProgress, throttle bool) bool {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	cp := p
	s.hub.job = &cp
	if throttle {
		now := time.Now()
		if !s.hub.lastProg.IsZero() && now.Sub(s.hub.lastProg) < progressMinInterval {
			return false
		}
		s.hub.lastProg = now
	}
	return true
}

func (s *Server) emitProgress(p jobProgress, throttle bool) {
	if !s.setJob(p, throttle) {
		return
	}
	s.hub.broadcast(eventProgress, p)
}

func (s *Server) emitDone(p jobProgress) {
	s.setJob(p, false)
	s.hub.broadcast(eventDone, p)
}

func (s *Server) emitFail(p jobProgress) {
	s.setJob(p, false)
	s.hub.broadcast(eventError, p)
}

// workspaceFiles lists the staging dir, degrading to an empty slice (with a
// log line) when the dir cannot be read.
func (s *Server) workspaceFiles() []workspaceFile {
	files, err := listWorkspaceFiles(s.workspaceDir())
	if err != nil {
		log.Println("list workspace:", err)
		return []workspaceFile{}
	}
	if files == nil {
		files = []workspaceFile{}
	}
	return files
}

func (s *Server) emitFiles() {
	s.hub.broadcast(eventFiles, map[string]any{"files": s.workspaceFiles()})
}

func (s *Server) emitState() {
	s.hub.broadcast(eventState, s.state.get())
}

func (s *Server) snapshot() eventsSnapshot {
	snap := eventsSnapshot{
		Files: s.workspaceFiles(),
		State: s.state.get(),
	}
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	snap.Lock = s.hub.lock
	if s.hub.job != nil {
		job := *s.hub.job
		snap.Job = &job
	}
	return snap
}

func writeSSE(w io.Writer, event string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

func (s *Server) handleUploadEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	if err := writeSSE(w, eventSnapshot, s.snapshot()); err != nil {
		return
	}
	flusher.Flush()

	tick := time.NewTicker(eventsHeartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				// Evicted by broadcast for falling behind; ending the
				// response lets EventSource reconnect to a fresh snapshot.
				return
			}
			if err := writeSSE(w, ev.Name, ev.Data); err != nil {
				return
			}
			flusher.Flush()
		case <-tick.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func workspaceBusy(w http.ResponseWriter) {
	http.Error(w, "workspace busy", http.StatusConflict)
}
