package main

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

type fileEntry struct {
	Name         string
	Key          string
	Size         string
	LastModified string
	SizeBytes    int64
	// Bundle detail extras: type icon, inline-preview kind ("image"/"video"/
	// "audio") and the signed preview URL (empty when preview is unavailable).
	Icon       string
	Kind       string
	PreviewURL string
}

type browseData struct {
	Nav string

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
	Time  string
	Title string
	ID    string
}

// calDayGroup is one day of the year list: all bundles under one YYYYMMDD
// date. The list is rendered newest days first.
type calDayGroup struct {
	Date     string // YYYY-MM-DD
	Bundles  []calDir
	Selected bool
	Today    bool
}

func entryName(full, prefix string) string {
	name := strings.TrimPrefix(full, prefix)
	return strings.TrimSuffix(name, "/")
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
		// Bundles arrive oldest-first; prepend so a day group lists its
		// bundles newest first, matching the newest-days-first group order.
		g.Bundles = append([]calDir{{
			Time:  when.Format("15:04"),
			Title: b.Title,
			ID:    strings.ToLower(b.ID),
		}}, g.Bundles...)
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
	return t.Local().Format("2006-01-02 15:04")
}
