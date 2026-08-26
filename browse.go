package main

import (
	"log"
	"slices"
	"strconv"
	"strings"
	"time"
)

type crumb struct {
	Name   string
	Prefix string
}

type dirEntry struct {
	Name   string
	Prefix string
}

type fileEntry struct {
	Name         string
	Key          string
	Size         string
	LastModified string
	SizeBytes    int64
	// Bundle view extras: type icon, inline-preview kind ("image"/"video"/
	// "audio") and the signed preview URL (empty when preview is unavailable).
	Icon       string
	Kind       string
	PreviewURL string
}

type browseData struct {
	Nav string

	// Contents mode (prefix view).
	Contents   bool
	Crumbs     []crumb
	Parent     string
	HasParent  bool
	Dirs       []dirEntry
	Files      []fileEntry
	NextMarker string
	Prefix     string

	// Bundle mode (dedicated view for a bundle directory).
	Bundle    bool
	Title     string
	Date      string // YYYY-MM-DD
	TimeHM    string // hh:mm
	BackMonth string // YYYY-MM
	FileCount int
	TotalSize string
	HasImages bool
	HasMedia  bool

	// Calendar mode.
	Month       string
	MonthLabel  string
	PrevMonth   string
	NextMonth   string
	Weeks       [][7]calDay
	SelectedDay string
	// SelectedMissing marks a selected day that has no bundles, so the year
	// list has no group to highlight for it.
	SelectedMissing bool
	Year            string
	DayGroups       []calDayGroup
}

// calDay is one calendar cell; Day == 0 is a blank filler cell.
type calDay struct {
	Day      int
	Date     string
	Bundles  int
	Today    bool
	Selected bool
}

// calDir is one bundle listed for a day.
type calDir struct {
	Time   string
	Title  string
	Prefix string
}

// calDayGroup is one day of the year list: all bundles under one YYYYMMDD
// date. The list is rendered newest days first.
type calDayGroup struct {
	Date     string // YYYY-MM-DD
	Bundles  []calDir
	Selected bool
	Today    bool
}

func normalizePrefix(p string) string {
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return ""
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func parentPrefix(prefix string) (string, bool) {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return "", false
	}
	i := strings.LastIndex(prefix, "/")
	if i < 0 {
		return "", true
	}
	return prefix[:i+1], true
}

func breadcrumbs(prefix string) []crumb {
	out := []crumb{{Name: "Home", Prefix: ""}}
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return out
	}
	acc := ""
	for _, part := range strings.Split(prefix, "/") {
		if part == "" {
			continue
		}
		acc += part + "/"
		out = append(out, crumb{Name: part, Prefix: acc})
	}
	return out
}

func entryName(full, prefix string) string {
	name := strings.TrimPrefix(full, prefix)
	return strings.TrimSuffix(name, "/")
}

// pageNextMarker falls back to the last listed object key or common prefix
// when the store truncates a page without reporting NextMarker.
func pageNextMarker(page ListPage) string {
	if !page.IsTruncated {
		return ""
	}
	if page.NextMarker != "" {
		return page.NextMarker
	}
	if n := len(page.Objects); n > 0 {
		return page.Objects[n-1].Key
	}
	if n := len(page.Prefixes); n > 0 {
		return page.Prefixes[n-1]
	}
	return ""
}

func buildBrowseData(prefix string, page ListPage) browseData {
	prefix = normalizePrefix(prefix)
	parent, hasParent := parentPrefix(prefix)
	data := browseData{
		Nav:        "browse",
		Crumbs:     breadcrumbs(prefix),
		Parent:     parent,
		HasParent:  hasParent,
		Prefix:     prefix,
		NextMarker: pageNextMarker(page),
	}
	for _, p := range page.Prefixes {
		name := entryName(p, prefix)
		if name == "" {
			continue
		}
		data.Dirs = append(data.Dirs, dirEntry{Name: name, Prefix: p})
	}
	for _, obj := range page.Objects {
		name := entryName(obj.Key, prefix)
		if name == "" {
			continue
		}
		data.Files = append(data.Files, fileEntry{
			Name:         name,
			Key:          obj.Key,
			Size:         formatSize(obj.Size),
			LastModified: formatTime(obj.LastModified),
			SizeBytes:    obj.Size,
		})
	}
	return data
}

// decorateBundle upgrades a contents listing of a bundle directory into the
// dedicated bundle view: header from the in-memory index (falling back to the
// bucket's .meta.json), file stats, type icons and signed inline-preview URLs
// for browser-native media. It is a no-op when the prefix is not a
// content/<aa>/<bb>/<uuid>/ bundle or no meta can be found.
func (s *Server) decorateBundle(data *browseData) {
	id, ok := parseBundleID(data.Prefix)
	if !ok {
		return
	}
	meta, ok := s.index.get(id)
	if !ok {
		if s.store == nil {
			return
		}
		// Not in the index (e.g. the bucket was written by another tool):
		// fall back to the bundle's own .meta.json.
		var err error
		if meta, err = loadBundleMeta(s.store, id); err != nil {
			return
		}
	}
	when, err := time.Parse(pushTimeLayout, meta.Time)
	if err != nil {
		return
	}
	data.Bundle = true
	data.Date = when.Format(browseDayLayout)
	data.TimeHM = when.Format("15:04")
	data.Title = meta.Title
	data.BackMonth = when.Format(browseMonthLayout)
	files := make([]fileEntry, 0, len(data.Files))
	for _, f := range data.Files {
		if f.Name == bundleMetaName {
			continue
		}
		files = append(files, f)
	}
	data.Files = files
	var total int64
	for i := range data.Files {
		f := &data.Files[i]
		f.Icon = fileIcon(f.Name)
		total += f.SizeBytes
		kind := previewKind(f.Name)
		if kind == "image" && f.SizeBytes > imagePreviewMaxSize {
			kind = ""
		}
		if kind == "" {
			continue
		}
		u, err := s.store.SignPreviewURL(f.Key, signURLTTL)
		if err != nil {
			log.Println("sign preview:", err)
			continue
		}
		f.Kind = kind
		f.PreviewURL = u
		if kind == "image" {
			data.HasImages = true
		} else {
			data.HasMedia = true
		}
	}
	data.FileCount = len(data.Files)
	data.TotalSize = formatSize(total)
}

const (
	browseMonthLayout = "2006-01"
	browseDayLayout   = "2006-01-02"
)

// buildBrowseCalendar renders the calendar for one month plus the year list
// of bundle day groups (newest first). yearBundles holds every bundle of the
// month's year from the monthly indexes; only that month's own entries count
// towards the calendar cells. A zero selected time shows the month without a
// selected day.
func buildBrowseCalendar(month time.Time, selected time.Time, yearBundles []bundleMeta, now time.Time) browseData {
	monthKey := month.Format(browseMonthLayout)
	selectedKey := ""
	if !selected.IsZero() {
		selectedKey = selected.Format("20060102")
	}
	todayKey := now.Format("20060102")
	counts := map[string]int{}
	groupsByKey := map[string]*calDayGroup{}
	var ordered []*calDayGroup
	sorted := append([]bundleMeta(nil), yearBundles...)
	slices.SortStableFunc(sorted, func(a, b bundleMeta) int {
		return strings.Compare(a.Time, b.Time)
	})
	for _, b := range sorted {
		if !isUUIDv4(strings.ToLower(b.ID)) {
			continue
		}
		when, err := time.Parse(pushTimeLayout, b.Time)
		if err != nil {
			continue
		}
		dayKey := when.Format("20060102")
		if strings.HasPrefix(b.Time, monthKey) {
			counts[dayKey]++
		}
		g := groupsByKey[dayKey]
		if g == nil {
			g = &calDayGroup{
				Date:     when.Format(browseDayLayout),
				Selected: dayKey == selectedKey,
				Today:    dayKey == todayKey,
			}
			groupsByKey[dayKey] = g
			ordered = append(ordered, g)
		}
		g.Bundles = append(g.Bundles, calDir{
			Time:   when.Format("15:04"),
			Title:  b.Title,
			Prefix: bundlePrefix(strings.ToLower(b.ID)) + "/",
		})
	}
	data := browseData{
		Nav:        "browse",
		Month:      month.Format(browseMonthLayout),
		MonthLabel: month.Format("January 2006"),
		PrevMonth:  month.AddDate(0, -1, 0).Format(browseMonthLayout),
		NextMonth:  month.AddDate(0, 1, 0).Format(browseMonthLayout),
		Year:       month.Format("2006"),
	}
	// yearBundles are sorted oldest-first; the year list shows newest days first.
	for i := len(ordered) - 1; i >= 0; i-- {
		data.DayGroups = append(data.DayGroups, *ordered[i])
	}
	if !selected.IsZero() {
		data.SelectedDay = selected.Format(browseDayLayout)
		if groupsByKey[selectedKey] == nil {
			data.SelectedMissing = true
		}
	}
	// Grid with weeks starting Monday; Day == 0 cells are blank fillers.
	first := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, month.Location())
	offset := (int(first.Weekday()) + 6) % 7
	daysInMonth := first.AddDate(0, 1, -1).Day()
	var week [7]calDay
	flush := func() {
		data.Weeks = append(data.Weeks, week)
		week = [7]calDay{}
	}
	for d := 1; d <= daysInMonth; d++ {
		date := time.Date(month.Year(), month.Month(), d, 0, 0, 0, 0, month.Location())
		key := date.Format("20060102")
		if col := (offset + d - 1) % 7; col == 0 && d > 1 {
			flush()
		}
		week[(offset+d-1)%7] = calDay{
			Day:      d,
			Date:     date.Format(browseDayLayout),
			Bundles:  counts[key],
			Today:    key == todayKey,
			Selected: key == selectedKey,
		}
	}
	flush()
	return data
}

func fileExt(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}

// fileIcon picks a Bootstrap Icons class from the file extension.
func fileIcon(name string) string {
	switch fileExt(name) {
	case "jpg", "jpeg", "png", "gif", "webp", "avif", "bmp", "svg", "ico", "heic":
		return "bi-file-earmark-image"
	case "mp4", "m4v", "webm", "mov", "mkv", "avi":
		return "bi-file-earmark-play"
	case "mp3", "wav", "ogg", "m4a", "flac", "aac":
		return "bi-file-earmark-music"
	case "pdf":
		return "bi-file-earmark-pdf"
	case "txt", "md", "log", "json", "yaml", "yml", "xml", "csv":
		return "bi-file-earmark-text"
	case "doc", "docx":
		return "bi-file-earmark-word"
	case "xls", "xlsx":
		return "bi-file-earmark-excel"
	case "ppt", "pptx":
		return "bi-file-earmark-ppt"
	case "zip", "tar", "gz", "tgz", "rar", "7z":
		return "bi-file-earmark-zip"
	default:
		return "bi-file-earmark"
	}
}

// previewKind reports the inline player for browser-native media; other files
// stay download-only.
func previewKind(name string) string {
	switch fileExt(name) {
	case "jpg", "jpeg", "png", "gif", "webp", "avif", "bmp":
		return "image"
	case "mp4", "m4v", "webm":
		return "video"
	case "mp3", "wav", "ogg", "m4a", "flac", "aac":
		return "audio"
	default:
		return ""
	}
}

// imagePreviewMaxSize caps inline image previews; larger images keep the
// download-only row so the page stays light.
const imagePreviewMaxSize = 32 << 20

func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	suffix := "KMGTPE"[exp]
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + string(suffix) + "B"
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04")
}
