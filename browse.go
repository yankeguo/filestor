package main

import (
	"log"
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
	DayDirs     []calDir
}

// calDay is one calendar cell; Day == 0 is a blank filler cell.
type calDay struct {
	Day      int
	Date     string
	Bundles  int
	Today    bool
	Selected bool
}

// calDir is one bundle listed for the selected day.
type calDir struct {
	Time   string
	Title  string
	Prefix string
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

// A bundle is a directory named YYYYMMDDhhmm-TITLE under the fixed YYYY/MM/
// layout: one pushed batch of files at a picked wall-clock time.
//
// parseBundlePrefix matches the fixed bundle layout YYYY/MM/YYYYMMDDhhmm-TITLE/
// and returns the bundle's display date (YYYY-MM-DD), time (hh:mm) and title.
func parseBundlePrefix(prefix string) (date, hm, title string, ok bool) {
	parts := strings.Split(strings.Trim(prefix, "/"), "/")
	if len(parts) != 3 || len(parts[0]) != 4 || !isDigits(parts[0]) ||
		len(parts[1]) != 2 || !isDigits(parts[1]) {
		return "", "", "", false
	}
	hm, title = splitDayDir(parts[2])
	if hm == "" {
		return "", "", "", false
	}
	name := parts[2]
	date = name[:4] + "-" + name[4:6] + "-" + name[6:8]
	return date, hm, title, true
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

// decorateBundle upgrades a contents listing of a bundle directory into the
// dedicated bundle view: parsed header, file stats, type icons and signed
// inline-preview URLs for browser-native media. It is a no-op for prefixes
// outside the fixed bundle layout.
func decorateBundle(data *browseData, store ObjectStore) {
	date, hm, title, ok := parseBundlePrefix(data.Prefix)
	if !ok {
		return
	}
	data.Bundle = true
	data.Date = date
	data.TimeHM = hm
	data.Title = title
	data.BackMonth = date[:7]
	var total int64
	for i := range data.Files {
		f := &data.Files[i]
		f.Icon = fileIcon(f.Name)
		total += f.SizeBytes
		kind := previewKind(f.Name)
		if kind == "image" && f.SizeBytes > imagePreviewMaxSize {
			kind = ""
		}
		if kind == "" || store == nil {
			continue
		}
		u, err := store.SignPreviewURL(f.Key, signURLTTL)
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
	// monthListMaxPages bounds the pagination followed to collect one month's
	// bundle directories (at 200 keys per page, 10 pages is 2000 directories).
	monthListMaxPages = 10
)

// listAllDirs returns every common prefix under prefix, following pagination
// so the calendar sees the whole month even when it holds many bundles.
func listAllDirs(store ObjectStore, prefix string) ([]string, error) {
	var dirs []string
	marker := ""
	for range monthListMaxPages {
		page, err := store.List(prefix, marker)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, page.Prefixes...)
		marker = pageNextMarker(page)
		if marker == "" {
			break
		}
	}
	return dirs, nil
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// splitDayDir parses a directory name like 202608240659-weekly-report into a
// display time (06:59) and title. Non-conforming names are shown as-is.
func splitDayDir(name string) (hm, title string) {
	if len(name) > 13 && isDigits(name[:12]) && name[12] == '-' {
		return name[8:10] + ":" + name[10:12], name[13:]
	}
	return "", name
}

// buildBrowseCalendar renders one month of the fixed bundle layout
// (YYYY/MM/YYYYMMDDhhmm-TITLE/): dirs are the common prefixes under the
// month's "YYYY/MM/" prefix.
// A zero selected time shows the month without a day list.
func buildBrowseCalendar(month time.Time, selected time.Time, dirs []string, now time.Time) browseData {
	monthPrefix := month.Format("2006/01/")
	selectedKey := ""
	if !selected.IsZero() {
		selectedKey = selected.Format("20060102")
	}
	counts := map[string]int{}
	var dayDirs []calDir
	for _, full := range dirs {
		name := entryName(full, monthPrefix)
		if len(name) < 8 || !isDigits(name[:8]) {
			continue
		}
		counts[name[:8]]++
		if name[:8] == selectedKey {
			hm, title := splitDayDir(name)
			dayDirs = append(dayDirs, calDir{Time: hm, Title: title, Prefix: full})
		}
	}
	data := browseData{
		Nav:        "browse",
		Month:      month.Format(browseMonthLayout),
		MonthLabel: month.Format("January 2006"),
		PrevMonth:  month.AddDate(0, -1, 0).Format(browseMonthLayout),
		NextMonth:  month.AddDate(0, 1, 0).Format(browseMonthLayout),
		DayDirs:    dayDirs,
	}
	if !selected.IsZero() {
		data.SelectedDay = selected.Format(browseDayLayout)
	}
	// Grid with weeks starting Monday; Day == 0 cells are blank fillers.
	first := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, month.Location())
	offset := (int(first.Weekday()) + 6) % 7
	daysInMonth := first.AddDate(0, 1, -1).Day()
	todayKey := now.Format("20060102")
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
