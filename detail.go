package main

import (
	"errors"
	"log"
	"net/http"
	"net/url"
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
// inline previews for browser-native media. A non-UUID id is a 400, an
// unknown bundle a 404.
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
			if errors.Is(err, errNotFound) {
				http.Error(w, "bundle not found", http.StatusNotFound)
				return
			}
			log.Println("load bundle meta:", err)
			http.Error(w, "meta lookup failed", http.StatusBadGateway)
			return
		}
	}
	when, err := time.Parse(pushTimeLayout, meta.Time)
	if err != nil {
		http.Error(w, "bundle not found", http.StatusNotFound)
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
	// Page through the whole bundle prefix: one List call only returns the
	// first page, so a large bundle would be truncated.
	prefix := bundlePrefix(id) + "/"
	var total int64
	marker := ""
	for {
		page, err := s.store.List(prefix, marker)
		if err != nil {
			log.Println("list objects:", err)
			http.Error(w, "list failed", http.StatusBadGateway)
			return
		}
		for _, obj := range page.Objects {
			name := entryName(obj.Key, prefix)
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
				// Previews link the same-origin /preview route, which signs
				// and redirects per request: render-time presigned URLs
				// would expire long before a lazy-loaded image is scrolled
				// into view or a media player is started.
				f.Kind = kind
				f.PreviewURL = "/preview?key=" + url.QueryEscape(obj.Key)
				if kind == "image" {
					data.HasImages = true
				} else {
					data.HasMedia = true
				}
			}
			data.Files = append(data.Files, f)
		}
		// A truncated page with no objects only happens when common prefixes
		// fill the page; a bundle has at most the .digest/ prefix, so the
		// last object key always advances the marker.
		if !page.IsTruncated || len(page.Objects) == 0 {
			break
		}
		marker = page.Objects[len(page.Objects)-1].Key
	}
	data.FileCount = len(data.Files)
	data.TotalSize = formatSize(total)
	s.render(w, "bundle.html", data)
}
