package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func awaitIdle(t *testing.T, s *Server) {
	t.Helper()
	require.Eventually(t, func() bool {
		return s.lockKind() == ""
	}, 5*time.Second, 10*time.Millisecond)
}

func readSSEEvent(t *testing.T, r *bufio.Reader) (name, data string) {
	t.Helper()
	var dataLines []string
	for {
		line, err := r.ReadString('\n')
		require.NoError(t, err)
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if name != "" || len(dataLines) > 0 {
				return name, strings.Join(dataLines, "\n")
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "event: "); ok {
			name = rest
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data: "); ok {
			dataLines = append(dataLines, rest)
		}
	}
}

func TestUploadEventsRequiresLogin(t *testing.T) {
	h := NewServer(cfgWithWorkspace(t), &fakeStore{}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/upload/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))
}

func TestUploadEventsSnapshotAndCancel(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Upload.Workspace, "a.txt"), []byte("hi"), 0o644))
	require.NoError(t, saveWorkspaceState(cfg.Upload.Workspace, workspaceState{Time: "2026-08-24T06:59", Title: "draft"}))
	h := NewServer(cfg, &fakeStore{}).Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	cookie := loginCookie(t, h)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/upload/events", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
	require.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))

	name, data := readSSEEvent(t, bufio.NewReader(resp.Body))
	require.Equal(t, eventSnapshot, name)
	var snap eventsSnapshot
	require.NoError(t, json.Unmarshal([]byte(data), &snap))
	require.Empty(t, snap.Lock)
	require.Len(t, snap.Files, 1)
	require.Equal(t, "a.txt", snap.Files[0].Name)
	require.Equal(t, "draft", snap.State.Title)

	cancel()
	_, _ = io.Copy(io.Discard, resp.Body)
}

func TestUploadEventsBroadcastsFiles(t *testing.T) {
	cfg := cfgWithWorkspace(t)
	h := NewServer(cfg, &fakeStore{}).Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	cookie := loginCookie(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/upload/events", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	name, _ := readSSEEvent(t, r)
	require.Equal(t, eventSnapshot, name)

	postUploadFile(t, h, cookie, "n.txt")

	gotLock := false
	gotFiles := false
	for !gotLock || !gotFiles {
		name, data := readSSEEvent(t, r)
		switch name {
		case eventLock:
			var payload struct {
				Lock string `json:"lock"`
			}
			require.NoError(t, json.Unmarshal([]byte(data), &payload))
			if payload.Lock == lockStage {
				gotLock = true
			}
		case eventFiles:
			var payload struct {
				Files []workspaceFile `json:"files"`
			}
			require.NoError(t, json.Unmarshal([]byte(data), &payload))
			if len(payload.Files) == 1 && payload.Files[0].Name == "n.txt" {
				gotFiles = true
			}
		}
	}
	require.True(t, gotLock, "missing stage lock event")
	require.True(t, gotFiles, "missing files event")
}
