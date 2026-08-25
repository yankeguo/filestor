package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type putCall struct {
	Key  string
	Data []byte
}

type fakeStore struct {
	page     ListPage
	signed   string
	err      error
	listMu   sync.Mutex
	lastList [2]string
	lastKey  string
	listFn   func(prefix, marker string) (ListPage, error)

	previewSigned  string
	lastPreviewKey string

	putErr   error
	putHook  func(key string)
	putBlock chan struct{}
	putsMu   sync.Mutex
	puts     []putCall
}

func (f *fakeStore) List(prefix, marker string) (ListPage, error) {
	f.listMu.Lock()
	f.lastList = [2]string{prefix, marker}
	f.listMu.Unlock()
	if f.listFn != nil {
		return f.listFn(prefix, marker)
	}
	return f.page, f.err
}

// lastListCall returns the most recent List call (which month finished last
// is nondeterministic for the year fan-out, so calendar tests must not rely
// on it).
func (f *fakeStore) lastListCall() [2]string {
	f.listMu.Lock()
	defer f.listMu.Unlock()
	return f.lastList
}

func (f *fakeStore) SignGetURL(key string, ttl time.Duration) (string, error) {
	f.lastKey = key
	if f.err != nil {
		return "", f.err
	}
	if f.signed != "" {
		return f.signed, nil
	}
	return "https://example.oss-cn-hangzhou.aliyuncs.com/" + url.PathEscape(key) + "?sig=1", nil
}

func (f *fakeStore) SignPreviewURL(key string, ttl time.Duration) (string, error) {
	f.lastPreviewKey = key
	if f.err != nil {
		return "", f.err
	}
	if f.previewSigned != "" {
		return f.previewSigned, nil
	}
	return "https://example.oss-cn-hangzhou.aliyuncs.com/preview/" + url.PathEscape(key) + "?sig=1", nil
}

func (f *fakeStore) Put(key string, r io.Reader, size int64) error {
	if f.putHook != nil {
		f.putHook(key)
	}
	if f.putBlock != nil {
		<-f.putBlock
	}
	if f.putErr != nil {
		return f.putErr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.putsMu.Lock()
	f.puts = append(f.puts, putCall{Key: key, Data: b})
	f.putsMu.Unlock()
	return nil
}

func (f *fakeStore) putKeys() []string {
	f.putsMu.Lock()
	defer f.putsMu.Unlock()
	keys := make([]string, 0, len(f.puts))
	for _, p := range f.puts {
		keys = append(keys, p.Key)
	}
	return keys
}

func TestBrowseCalendarLanding(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/browse", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, ">Mo</th>")
	require.Contains(t, body, time.Now().Format("January 2006"))
	// Landing selects today.
	require.Contains(t, body, time.Now().Format(browseDayLayout))
}

func TestBrowseCalendarHighlightsAndListsDay(t *testing.T) {
	store := &fakeStore{listFn: func(prefix, marker string) (ListPage, error) {
		if prefix != "2026/08/" {
			return ListPage{}, nil
		}
		return ListPage{Prefixes: []string{
			"2026/08/202608240659-weekly-report/",
			"2026/08/202608241200-monthly-billing/",
			"2026/08/202608101015-发票/",
			"2026/08/legacy-dir/",
		}}, nil
	}}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/browse?month=2026-08&day=2026-08-24", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "August 2026")
	// Days with bundles link back to the day view, anchored to the year list.
	require.Contains(t, body, `/browse?month=2026-08&day=2026-08-24#day-2026-08-24`)
	require.Contains(t, body, `day=2026-08-10#day-2026-08-10`)
	// Non-conforming directories are not counted.
	require.NotContains(t, body, "legacy-dir")
	// The year list groups every day of the year with parsed time and title.
	require.Contains(t, body, `id="day-2026-08-24"`)
	require.Contains(t, body, `id="day-2026-08-10"`)
	require.Contains(t, body, "06:59")
	require.Contains(t, body, "weekly-report")
	require.Contains(t, body, "12:00")
	require.Contains(t, body, "monthly-billing")
	require.Contains(t, body, `/browse?prefix=2026%2f08%2f202608240659-weekly-report%2f`)
	// Other days of the year appear too (not only the selected one).
	require.Contains(t, body, "10:15")
	require.Contains(t, body, "发票")
}

func TestBrowseCalendarDayOutsideMonthDropped(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	req := httptest.NewRequest(http.MethodGet, "/browse?month=2026-09&day=2026-08-24", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "September 2026")
	// The dropped day selects nothing: no group and no hint.
	require.NotContains(t, body, "2026-08-24")
	require.NotContains(t, body, `class="browse-day`)
	require.Contains(t, body, "No bundles this year.")
}

func TestBrowseCalendarFollowsPagination(t *testing.T) {
	type call struct{ prefix, marker string }
	var mu sync.Mutex
	var calls []call
	store := &fakeStore{listFn: func(prefix, marker string) (ListPage, error) {
		mu.Lock()
		calls = append(calls, call{prefix, marker})
		mu.Unlock()
		if prefix != "2026/08/" {
			return ListPage{}, nil
		}
		if marker == "" {
			return ListPage{
				Prefixes:    []string{"2026/08/202608011200-a/"},
				IsTruncated: true,
				NextMarker:  "2026/08/202608011200-a/",
			}, nil
		}
		return ListPage{Prefixes: []string{"2026/08/202608311200-b/"}}, nil
	}}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)
	req := httptest.NewRequest(http.MethodGet, "/browse?month=2026-08", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "day=2026-08-01")
	require.Contains(t, body, "day=2026-08-31")
	// Pagination resumes from the last listed prefix; the year fan-out lists
	// every other month once, without a marker.
	require.Contains(t, calls, call{"2026/08/", ""})
	require.Contains(t, calls, call{"2026/08/", "2026/08/202608011200-a/"})
	for _, c := range calls {
		if c.prefix != "2026/08/" {
			require.Empty(t, c.marker, c.prefix)
		}
	}
}

func TestSplitDayDir(t *testing.T) {
	hm, title := splitDayDir("202608240659-weekly-report")
	require.Equal(t, "06:59", hm)
	require.Equal(t, "weekly-report", title)

	hm, title = splitDayDir("legacy-dir")
	require.Equal(t, "", hm)
	require.Equal(t, "legacy-dir", title)
}

func TestBuildBrowseCalendarGrid(t *testing.T) {
	// August 2026 starts on a Saturday and has 31 days.
	month := time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.Local)
	yearDirs := []string{
		"2026/07/202607101200-y/",
		"2026/08/202608240659-x/",
		"2026/08/legacy-dir/",
	}
	d := buildBrowseCalendar(month, now, yearDirs, now)
	require.Equal(t, "2026-08", d.Month)
	require.Equal(t, "2026-07", d.PrevMonth)
	require.Equal(t, "2026-09", d.NextMonth)
	require.Equal(t, "2026", d.Year)
	// Monday-first grid: the 1st (Saturday) sits in column 5.
	require.Equal(t, 1, d.Weeks[0][5].Day)
	require.Equal(t, 0, d.Weeks[0][4].Day)
	// The 31st lands in the first column of the last week.
	last := d.Weeks[len(d.Weeks)-1]
	require.Equal(t, 31, last[0].Day)
	// The selected day is flagged, highlighted with its bundle, and listed.
	// (2026-08-24 is the Monday of the fifth row.)
	sel := d.Weeks[4][0]
	require.Equal(t, 24, sel.Day)
	require.True(t, sel.Selected)
	require.True(t, sel.Today)
	require.Equal(t, 1, sel.Bundles)
	// Only the month's own dirs count towards the calendar cells.
	for _, week := range d.Weeks {
		for _, day := range week {
			if day.Day == 10 {
				require.Zero(t, day.Bundles)
			}
		}
	}
	// The year list groups every day of the year, newest first; the selected
	// (also today) group is flagged.
	require.Equal(t, []calDayGroup{
		{Date: "2026-08-24", Bundles: []calDir{{Time: "06:59", Title: "x", Prefix: "2026/08/202608240659-x/"}}, Selected: true, Today: true},
		{Date: "2026-07-10", Bundles: []calDir{{Time: "12:00", Title: "y", Prefix: "2026/07/202607101200-y/"}}},
	}, d.DayGroups)
	require.False(t, d.SelectedMissing)
}

func TestBuildBrowseCalendarSelectedMissing(t *testing.T) {
	month := time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.Local)
	selected := time.Date(2026, 8, 10, 0, 0, 0, 0, time.Local)
	d := buildBrowseCalendar(month, selected, []string{"2026/08/202608240659-x/"}, now)
	require.Equal(t, "2026-08-10", d.SelectedDay)
	require.True(t, d.SelectedMissing)
}

func TestListYearDirs(t *testing.T) {
	store := &fakeStore{listFn: func(prefix, marker string) (ListPage, error) {
		switch prefix {
		case "2026/03/":
			return ListPage{Prefixes: []string{"2026/03/202603011200-a/"}}, nil
		case "2026/11/":
			return ListPage{Prefixes: []string{"2026/11/202611022359-b/", "2026/11/202611030100-c/"}}, nil
		default:
			return ListPage{}, nil
		}
	}}
	dirs, err := listYearDirs(store, 2026)
	require.NoError(t, err)
	// Months concatenate in calendar order.
	require.Equal(t, []string{
		"2026/03/202603011200-a/",
		"2026/11/202611022359-b/",
		"2026/11/202611030100-c/",
	}, dirs)

	store.err = errors.New("boom")
	store.listFn = nil
	_, err = listYearDirs(store, 2026)
	require.Error(t, err)
}

func TestBrowsePrefixAndParent(t *testing.T) {
	store := &fakeStore{page: ListPage{
		Objects: []ObjectInfo{{Key: "docs/a.txt", Size: 1}},
	}}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/browse?prefix=docs", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "docs/", store.lastListCall()[0])
	require.Contains(t, rec.Body.String(), `href="/browse"`)
	require.Contains(t, rec.Body.String(), "a.txt")
	require.Contains(t, rec.Body.String(), `/download?key=docs%2fa.txt`)
}

func TestBrowseNextPage(t *testing.T) {
	store := &fakeStore{page: ListPage{
		Objects:     []ObjectInfo{{Key: "a.txt", Size: 1}},
		IsTruncated: true,
		NextMarker:  "a.txt",
	}}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/browse?prefix=docs", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Contains(t, rec.Body.String(), "marker=a.txt")
}

func TestBrowseBundleView(t *testing.T) {
	store := &fakeStore{page: ListPage{Objects: []ObjectInfo{
		{Key: "2026/08/202608240659-weekly-report/photo.jpg", Size: 1024},
		{Key: "2026/08/202608240659-weekly-report/clip.mp4", Size: 2048},
		{Key: "2026/08/202608240659-weekly-report/voice.mp3", Size: 512},
		{Key: "2026/08/202608240659-weekly-report/notes.pdf", Size: 512},
	}}}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/browse?prefix=2026/08/202608240659-weekly-report/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "2026/08/202608240659-weekly-report/", store.lastListCall()[0])
	body := rec.Body.String()
	// Dedicated header: parsed title, date and time, back link to the day.
	require.Contains(t, body, "<h4 class=\"mb-1\">weekly-report</h4>")
	require.Contains(t, body, "2026-08-24")
	require.Contains(t, body, "06:59")
	require.Contains(t, body, `/browse?month=2026-08&day=2026-08-24`)
	// Stats line.
	require.Contains(t, body, "4 files")
	// Inline previews for browser-native media, signed via the store.
	require.Contains(t, body, "<img")
	require.Contains(t, body, "<video")
	require.Contains(t, body, "<audio")
	require.Contains(t, body, "example.oss-cn-hangzhou.aliyuncs.com/preview/")
	// Non-previewable files keep a typed icon and download-only row.
	require.Contains(t, body, "bi-file-earmark-pdf")
	require.Contains(t, body, `/download?key=2026%2f08%2f202608240659-weekly-report%2fnotes.pdf`)
	// The generic breadcrumbs view is not rendered.
	require.NotContains(t, body, "breadcrumb")
}

func TestBrowseBundleViewSkipsOversizedImagePreview(t *testing.T) {
	store := &fakeStore{page: ListPage{Objects: []ObjectInfo{
		{Key: "2026/08/202608240659-x/big.jpg", Size: imagePreviewMaxSize + 1},
	}}}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/browse?prefix=2026/08/202608240659-x/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.NotContains(t, body, "<img")
	require.Contains(t, body, "bi-file-earmark-image")
}

func TestParseBundlePrefix(t *testing.T) {
	date, hm, title, ok := parseBundlePrefix("2026/08/202608240659-weekly-report/")
	require.True(t, ok)
	require.Equal(t, "2026-08-24", date)
	require.Equal(t, "06:59", hm)
	require.Equal(t, "weekly-report", title)

	for _, p := range []string{
		"",
		"docs/",
		"2026/08/",
		"2026/08/legacy-dir/",
		"2026/08/202608240659-x/extra/",
		"2x26/08/202608240659-x/",
	} {
		_, _, _, ok := parseBundlePrefix(p)
		require.False(t, ok, p)
	}
}

func TestPreviewKindAndFileIcon(t *testing.T) {
	require.Equal(t, "image", previewKind("a.JPG"))
	require.Equal(t, "video", previewKind("a.mp4"))
	require.Equal(t, "audio", previewKind("a.mp3"))
	require.Equal(t, "", previewKind("a.pdf"))
	require.Equal(t, "", previewKind("a"))

	require.Equal(t, "bi-file-earmark-image", fileIcon("a.png"))
	require.Equal(t, "bi-file-earmark-pdf", fileIcon("a.pdf"))
	require.Equal(t, "bi-file-earmark-word", fileIcon("a.docx"))
	require.Equal(t, "bi-file-earmark-zip", fileIcon("a.tar.gz"))
	require.Equal(t, "bi-file-earmark", fileIcon("a"))
}

func TestPreviewRedirectsToInlineSignedURL(t *testing.T) {
	store := &fakeStore{previewSigned: "https://oss.example/inline"}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/preview?key=docs/a.jpg", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "https://oss.example/inline", rec.Header().Get("Location"))
	require.Equal(t, "docs/a.jpg", store.lastPreviewKey)
}

func TestPreviewRequiresLogin(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/preview?key=a.jpg", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))
}

func TestDownloadRedirectsToSignedURL(t *testing.T) {
	store := &fakeStore{signed: "https://oss.example/signed-object"}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/download?key=docs/a.txt", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "https://oss.example/signed-object", rec.Header().Get("Location"))
	require.Equal(t, "docs/a.txt", store.lastKey)
}

func TestDownloadRejectsEmptyKey(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	req := httptest.NewRequest(http.MethodGet, "/download", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDownloadRejectsInvalidKey(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	req := httptest.NewRequest(http.MethodGet, "/download?key="+url.QueryEscape("a.txt\r\nX-Injected: yes"), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDownloadRequiresLogin(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/download?key=a.txt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))
}

func TestBrowseListError(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{err: fmt.Errorf("boom")}).Handler()
	cookie := loginCookie(t, h)
	req := httptest.NewRequest(http.MethodGet, "/browse", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestNormalizePrefixAndParent(t *testing.T) {
	require.Equal(t, "", normalizePrefix(""))
	require.Equal(t, "docs/", normalizePrefix("docs"))
	require.Equal(t, "docs/a/", normalizePrefix("/docs/a/"))

	p, ok := parentPrefix("")
	require.False(t, ok)
	require.Equal(t, "", p)

	p, ok = parentPrefix("docs/")
	require.True(t, ok)
	require.Equal(t, "", p)

	p, ok = parentPrefix("docs/a/")
	require.True(t, ok)
	require.Equal(t, "docs/", p)
}

func TestFormatSize(t *testing.T) {
	require.Equal(t, "12 B", formatSize(12))
	require.Equal(t, "1.0 KB", formatSize(1024))
	require.Equal(t, "1.5 KB", formatSize(1536))
}

func TestBuildBrowseDataNextMarker(t *testing.T) {
	d := buildBrowseData("", ListPage{
		IsTruncated: true,
		NextMarker:  "x",
		Objects:     []ObjectInfo{{Key: "a.txt"}},
	})
	require.Equal(t, "x", d.NextMarker)

	d = buildBrowseData("", ListPage{IsTruncated: false, NextMarker: "x"})
	require.Equal(t, "", d.NextMarker)

	d = buildBrowseData("", ListPage{
		IsTruncated: true,
		Objects:     []ObjectInfo{{Key: "a.txt"}, {Key: "b.txt"}},
	})
	require.Equal(t, "b.txt", d.NextMarker)

	d = buildBrowseData("", ListPage{
		IsTruncated: true,
		Prefixes:    []string{"docs/", "img/"},
	})
	require.Equal(t, "img/", d.NextMarker)
}

func TestAttachmentDisposition(t *testing.T) {
	d := attachmentDisposition(`dir/"q".txt`)
	require.Contains(t, d, `filename="q.txt"`)
	require.Contains(t, d, "filename*=UTF-8''")

	d = attachmentDisposition("dir/evil\r\nX.txt")
	require.NotContains(t, d, "\r")
	require.NotContains(t, d, "\n")
	require.Contains(t, d, `filename="evilX.txt"`)
}
