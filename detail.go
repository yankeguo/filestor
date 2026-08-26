package main

import (
	"log"
	"net/http"
	"strings"
	"time"
)

// detailData is the view model for the dedicated bundle page
// (GET /bundle/{id}).
type detailData struct {
	Nav string

	ID        string
	Title     string
	Date      string // YYYY-MM-DD
	TimeHM    string // hh:mm
	BackMonth string // YYYY-MM
	FileCount int
	TotalSize string
	HasImages bool
	HasMedia  bool
	Files     []fileEntry
}

// handleBundle renders one bundle by its UUID v4: meta from the in-memory
// index (falling back to the bucket's .meta.json), file stats, type icons and
// signed inline-preview URLs for browser-native media. Unknown or invalid
// bundles are a 404, not the generic contents view.
func (s *Server) handleBundle(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "object store unavailable", http.StatusServiceUnavailable)
		return
	}
	id := strings.ToLower(r.PathValue("id"))
	if !isUUIDv4(id) {
		http.Error(w, "invalid bundle id", http.StatusBadRequest)
		return
	}
	meta, ok := s.index.get(id)
	if !ok {
		// Not in the index (e.g. the bucket was written by another tool):
		// fall back to the bundle's own .meta.json.
		var err error
		if meta, err = loadBundleMeta(s.store, id); err != nil {
			http.Error(w, "bundle not found", http.StatusNotFound)
			return
		}
	}
	when, err := time.Parse(pushTimeLayout, meta.Time)
	if err != nil {
		http.Error(w, "bundle not found", http.StatusNotFound)
		return
	}
	page, err := s.store.List(bundlePrefix(id)+"/", "")
	if err != nil {
		log.Println("list objects:", err)
		http.Error(w, "list failed", http.StatusBadGateway)
		return
	}
	data := detailData{
		Nav:       "browse",
		ID:        id,
		Title:     meta.Title,
		Date:      when.Format(browseDayLayout),
		TimeHM:    when.Format("15:04"),
		BackMonth: when.Format(browseMonthLayout),
	}
	var total int64
	for _, obj := range page.Objects {
		name := entryName(obj.Key, bundlePrefix(id)+"/")
		if name == "" || name == bundleMetaName {
			continue
		}
		f := fileEntry{
			Name:         name,
			Key:          obj.Key,
			Size:         formatSize(obj.Size),
			LastModified: formatTime(obj.LastModified),
			SizeBytes:    obj.Size,
			Icon:         fileIcon(name),
		}
		total += obj.Size
		kind := previewKind(name)
		if kind == "image" && obj.Size > imagePreviewMaxSize {
			kind = ""
		}
		if kind != "" {
			u, err := s.store.SignPreviewURL(obj.Key, signURLTTL)
			if err != nil {
				log.Println("sign preview:", err)
			} else {
				f.Kind = kind
				f.PreviewURL = u
				if kind == "image" {
					data.HasImages = true
				} else {
					data.HasMedia = true
				}
			}
		}
		data.Files = append(data.Files, f)
	}
	data.FileCount = len(data.Files)
	data.TotalSize = formatSize(total)
	s.render(w, "bundle.html", data)
}
