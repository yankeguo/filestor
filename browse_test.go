package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
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
	// Paged listing: when listPageSize > 0, List serves listObjs sorted by
	// key, filtered to prefix, listPageSize per call, resuming after marker
	// (mirroring S3's StartAfter).
	listObjs     []ObjectInfo
	listPageSize int

	previewSigned  string
	lastPreviewKey string

	putErr   error
	putHook  func(key string)
	putBlock chan struct{}
	putsMu   sync.Mutex
	puts     []putCall
	objects  map[string][]byte
	getFn    func(key string) ([]byte, error)
}

func (f *fakeStore) List(prefix, marker string) (ListPage, error) {
	f.listMu.Lock()
	f.lastList = [2]string{prefix, marker}
	f.listMu.Unlock()
	if f.listFn != nil {
		return f.listFn(prefix, marker)
	}
	if f.listPageSize > 0 {
		objs := slices.Clone(f.listObjs)
		slices.SortFunc(objs, func(a, b ObjectInfo) int { return strings.Compare(a.Key, b.Key) })
		var rest []ObjectInfo
		for _, o := range objs {
			if strings.HasPrefix(o.Key, prefix) && o.Key > marker {
				rest = append(rest, o)
			}
		}
		if len(rest) > f.listPageSize {
			return ListPage{Objects: rest[:f.listPageSize], IsTruncated: true}, f.err
		}
		return ListPage{Objects: rest}, f.err
	}
	return f.page, f.err
}

func (f *fakeStore) ListKeys(prefix string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.putsMu.Lock()
	defer f.putsMu.Unlock()
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys, nil
}

func (f *fakeStore) Get(key string) ([]byte, error) {
	if f.getFn != nil {
		return f.getFn(key)
	}
	if f.err != nil {
		return nil, f.err
	}
	f.putsMu.Lock()
	defer f.putsMu.Unlock()
	if f.objects != nil {
		if b, ok := f.objects[key]; ok {
			return append([]byte(nil), b...), nil
		}
	}
	return nil, errNotFound
}

// lastListCall returns the most recent List call (only meaningful for tests
// that trigger exactly one List).
func (f *fakeStore) lastListCall() [2]string {
	f.listMu.Lock()
	defer f.listMu.Unlock()
	return f.lastList
}

func (f *fakeStore) SignGetURL(key string, ttl time.Duration) (string, error) {
	f.listMu.Lock()
	f.lastKey = key
	f.listMu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	if f.signed != "" {
		return f.signed, nil
	}
	return "https://example.oss-cn-hangzhou.aliyuncs.com/" + url.PathEscape(key) + "?sig=1", nil
}

func (f *fakeStore) SignPreviewURL(key string, ttl time.Duration) (string, error) {
	f.listMu.Lock()
	f.lastPreviewKey = key
	f.listMu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	if f.previewSigned != "" {
		return f.previewSigned, nil
	}
	return "https://example.oss-cn-hangzhou.aliyuncs.com/preview/" + url.PathEscape(key) + "?sig=1", nil
}

// lastSignedKey returns the key of the most recent SignGetURL call.
func (f *fakeStore) lastSignedKey() string {
	f.listMu.Lock()
	defer f.listMu.Unlock()
	return f.lastKey
}

// lastPreviewSignedKey returns the key of the most recent SignPreviewURL call.
func (f *fakeStore) lastPreviewSignedKey() string {
	f.listMu.Lock()
	defer f.listMu.Unlock()
	return f.lastPreviewKey
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
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	f.objects[key] = b
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

func mustIndexJSON(t *testing.T, entries ...bundleMeta) []byte {
	t.Helper()
	b, err := json.Marshal(entries)
	require.NoError(t, err)
	return b
}

func mustMetaJSON(t *testing.T, meta bundleMeta) []byte {
	t.Helper()
	b, err := json.Marshal(meta)
	require.NoError(t, err)
	return b
}

const (
	testBundleID1 = "550e8400-e29b-41d4-a716-446655440000"
	testBundleID2 = "a1b2c3d4-e5f6-4789-abcd-ef0123456789"
	testBundleID3 = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	testBundleID4 = "11111111-2222-4333-8444-555555555555"
	testBundleID5 = "22222222-3333-4444-8555-666666666666"
	testBundleID6 = "33333333-4444-4555-8666-777777777777"
)

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
	store := &fakeStore{objects: map[string][]byte{
		"index/2026/2026-08.json": mustIndexJSON(t,
			bundleMeta{ID: testBundleID1, Title: "weekly-report", Time: "2026-08-24T06:59"},
			bundleMeta{ID: testBundleID2, Title: "monthly-billing", Time: "2026-08-24T12:00"},
			bundleMeta{ID: testBundleID3, Title: "发票", Time: "2026-08-10T10:15"},
		),
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
	// The year list groups every day of the year with parsed time and title.
	require.Contains(t, body, `id="day-2026-08-24"`)
	require.Contains(t, body, `id="day-2026-08-10"`)
	require.Contains(t, body, "06:59")
	require.Contains(t, body, "weekly-report")
	require.Contains(t, body, "12:00")
	require.Contains(t, body, "monthly-billing")
	require.Contains(t, body, `/bundle/550e8400-e29b-41d4-a716-446655440000`)
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

func TestBrowseCalendarMissingMonthsAreEmpty(t *testing.T) {
	store := &fakeStore{objects: map[string][]byte{
		"index/2026/2026-08.json": mustIndexJSON(t,
			bundleMeta{ID: testBundleID4, Title: "a", Time: "2026-08-01T12:00"},
			bundleMeta{ID: testBundleID5, Title: "b", Time: "2026-08-31T12:00"},
		),
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
}

func TestBuildBrowseCalendarGrid(t *testing.T) {
	// August 2026 starts on a Saturday and has 31 days.
	month := time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.Local)
	yearBundles := []bundleMeta{
		{ID: testBundleID4, Title: "y", Time: "2026-07-10T12:00"},
		{ID: testBundleID1, Title: "x", Time: "2026-08-24T06:59"},
		{ID: "not-a-uuid", Title: "skip", Time: "2026-08-10T00:00"},
	}
	d := buildBrowseCalendar(month, now, yearBundles, now)
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
	// Only the month's own entries count towards the calendar cells.
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
		{Date: "2026-08-24", Bundles: []calDir{{Time: "06:59", Title: "x", ID: testBundleID1}}, Selected: true, Today: true},
		{Date: "2026-07-10", Bundles: []calDir{{Time: "12:00", Title: "y", ID: testBundleID4}}},
	}, d.DayGroups)
	require.False(t, d.SelectedMissing)
}

func TestBuildBrowseCalendarSelectedMissing(t *testing.T) {
	month := time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.Local)
	selected := time.Date(2026, 8, 10, 0, 0, 0, 0, time.Local)
	d := buildBrowseCalendar(month, selected, []bundleMeta{
		{ID: testBundleID1, Title: "x", Time: "2026-08-24T06:59"},
	}, now)
	require.Equal(t, "2026-08-10", d.SelectedDay)
	require.True(t, d.SelectedMissing)
}

func TestBrowseCalendarReadsMemoryIndex(t *testing.T) {
	// The calendar is served from the in-memory index loaded at startup, so
	// it must not List or Get anything per request.
	store := &fakeStore{objects: map[string][]byte{
		"index/2026/2026-03.json": mustIndexJSON(t,
			bundleMeta{ID: testBundleID4, Title: "a", Time: "2026-03-01T12:00"},
		),
		"index/2026/2026-11.json": mustIndexJSON(t,
			bundleMeta{ID: testBundleID5, Title: "b", Time: "2026-11-02T23:59"},
			bundleMeta{ID: testBundleID6, Title: "c", Time: "2026-11-03T01:00"},
		),
	}}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)
	store.err = errors.New("boom")
	store.getFn = func(string) ([]byte, error) { return nil, errors.New("boom") }

	req := httptest.NewRequest(http.MethodGet, "/browse?month=2026-11", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// All three indexed days of the year are listed from memory.
	require.Contains(t, body, `id="day-2026-03-01"`)
	require.Contains(t, body, `id="day-2026-11-02"`)
	require.Contains(t, body, `id="day-2026-11-03"`)
	// The displayed month's own days are highlighted in the calendar grid.
	require.Contains(t, body, `/browse?month=2026-11&day=2026-11-02#day-2026-11-02`)
}

func TestBundleDetail(t *testing.T) {
	prefix := bundlePrefix(testBundleID1) + "/"
	meta := bundleMeta{ID: testBundleID1, Title: "weekly-report", Time: "2026-08-24T06:59"}
	store := &fakeStore{
		page: ListPage{Objects: []ObjectInfo{
			{Key: prefix + bundleMetaName, Size: 64},
			{Key: prefix + "photo.jpg", Size: 1024},
			{Key: prefix + "clip.mp4", Size: 2048},
			{Key: prefix + "voice.mp3", Size: 512},
			{Key: prefix + "notes.pdf", Size: 512},
		}},
		objects: map[string][]byte{bundleMetaKey(testBundleID1): mustMetaJSON(t, meta)},
	}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/bundle/"+testBundleID1, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, prefix, store.lastListCall()[0])
	body := rec.Body.String()
	// Dedicated header: title, date and time from .meta.json, back link to the day.
	require.Contains(t, body, "<h4 class=\"mb-1\">weekly-report</h4>")
	require.Contains(t, body, "2026-08-24")
	require.Contains(t, body, "06:59")
	require.Contains(t, body, `/browse?month=2026-08&day=2026-08-24`)
	// Stats line; .meta.json is omitted.
	require.Contains(t, body, "4 files")
	require.NotContains(t, body, bundleMetaName)
	// Inline previews for browser-native media, signed via the store.
	require.Contains(t, body, "<img")
	require.Contains(t, body, "<video")
	require.Contains(t, body, "<audio")
	require.Contains(t, body, "example.oss-cn-hangzhou.aliyuncs.com/preview/")
	// Non-previewable files keep a typed icon and download-only row.
	require.Contains(t, body, "bi-file-earmark-pdf")
	require.Contains(t, body, `/download?key=content%2f55%2f0e%2f550e8400-e29b-41d4-a716-446655440000%2fnotes.pdf`)
	// The generic breadcrumbs view is not rendered.
	require.NotContains(t, body, "breadcrumb")
}

func TestBundleDetailSkipsOversizedImagePreview(t *testing.T) {
	prefix := bundlePrefix(testBundleID1) + "/"
	meta := bundleMeta{ID: testBundleID1, Title: "x", Time: "2026-08-24T06:59"}
	store := &fakeStore{
		page: ListPage{Objects: []ObjectInfo{
			{Key: prefix + "big.jpg", Size: imagePreviewMaxSize + 1},
		}},
		objects: map[string][]byte{bundleMetaKey(testBundleID1): mustMetaJSON(t, meta)},
	}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/bundle/"+testBundleID1, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.NotContains(t, body, "<img")
	require.Contains(t, body, "bi-file-earmark-image")
}

func TestBundleDetailNotFound(t *testing.T) {
	store := &fakeStore{page: ListPage{Objects: []ObjectInfo{
		{Key: bundlePrefix(testBundleID1) + "/notes.pdf", Size: 512},
	}}}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	// Unknown id: not in the in-memory index, no .meta.json in the bucket.
	req := httptest.NewRequest(http.MethodGet, "/bundle/"+testBundleID1, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// Invalid id: not a UUID v4.
	req = httptest.NewRequest(http.MethodGet, "/bundle/not-a-uuid", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestParseBundleID(t *testing.T) {
	id, ok := parseBundleID("content/55/0e/550e8400-e29b-41d4-a716-446655440000/")
	require.True(t, ok)
	require.Equal(t, testBundleID1, id)

	for _, p := range []string{
		"",
		"docs/",
		"content/",
		"content/55/0e/",
		"bundles/55/0e/550e8400-e29b-41d4-a716-446655440000/",
		"content/55/0f/550e8400-e29b-41d4-a716-446655440000/",
		"content/55/0e/550e8400-e29b-41d4-a716-446655440000/extra/",
		"2026/08/202608240659-weekly-report/",
	} {
		_, ok := parseBundleID(p)
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
	require.Equal(t, "docs/a.jpg", store.lastPreviewSignedKey())
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
	require.Equal(t, "docs/a.txt", store.lastSignedKey())
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

func TestFormatSize(t *testing.T) {
	require.Equal(t, "12 B", formatSize(12))
	require.Equal(t, "1.0 KB", formatSize(1024))
	require.Equal(t, "1.5 KB", formatSize(1536))
}

func TestBundleRequiresLogin(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/bundle/"+testBundleID1, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))
}

func TestHandlersWithoutStore(t *testing.T) {
	// NewServer(cfg, nil) serves the auth-locked routes but every handler that
	// needs the bucket answers 503.
	h := NewServer(cfgWithWorkspace(t), nil).Handler()
	cookie := loginCookie(t, h)

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	require.Equal(t, http.StatusServiceUnavailable, get("/browse").Code)
	require.Equal(t, http.StatusServiceUnavailable, get("/download?key=x").Code)
	require.Equal(t, http.StatusServiceUnavailable, get("/preview?key=x").Code)
	require.Equal(t, http.StatusServiceUnavailable, get("/bundle/"+testBundleID1).Code)
	require.Equal(t, http.StatusServiceUnavailable, postPush(t, h, cookie, "2026-08-24T06:59", "t").Code)
}

func TestDownloadSignFailure(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{err: errors.New("sign down")}).Handler()
	cookie := loginCookie(t, h)
	req := httptest.NewRequest(http.MethodGet, "/download?key=a.txt", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestPreviewSignFailure(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{err: errors.New("sign down")}).Handler()
	cookie := loginCookie(t, h)
	req := httptest.NewRequest(http.MethodGet, "/preview?key=a.jpg", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestBundleDetailPaginatesList(t *testing.T) {
	prefix := bundlePrefix(testBundleID1) + "/"
	meta := bundleMeta{ID: testBundleID1, Title: "x", Time: "2026-08-24T06:59"}
	// 250 files plus .meta.json: two pages at a page size of 200.
	objs := make([]ObjectInfo, 0, 251)
	objs = append(objs, ObjectInfo{Key: prefix + bundleMetaName, Size: 64})
	for i := 0; i < 250; i++ {
		objs = append(objs, ObjectInfo{Key: fmt.Sprintf("%sf-%03d.txt", prefix, i), Size: 10})
	}
	store := &fakeStore{
		listObjs:     objs,
		listPageSize: 200,
		objects:      map[string][]byte{bundleMetaKey(testBundleID1): mustMetaJSON(t, meta)},
	}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/bundle/"+testBundleID1, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// Every file from both pages is listed; .meta.json stays omitted.
	require.Contains(t, body, "250 files")
	require.Contains(t, body, "f-000.txt")
	require.Contains(t, body, "f-249.txt")
	require.NotContains(t, body, bundleMetaName)
	// The second page resumed from the last key of the first page
	// (.meta.json sorts before f-000.txt).
	require.Equal(t, [2]string{prefix, prefix + "f-198.txt"}, store.lastListCall())
}

func TestBundleDetailMetaFallbackErrors(t *testing.T) {
	// The bundle is not in the in-memory index, so the meta comes from the
	// bucket: errNotFound is a 404, any other lookup failure a 502.
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", errNotFound, http.StatusNotFound},
		{"upstream error", errors.New("oss down"), http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{getFn: func(string) ([]byte, error) { return nil, tc.err }}
			h := NewServer(testCfg(), store).Handler()
			cookie := loginCookie(t, h)
			req := httptest.NewRequest(http.MethodGet, "/bundle/"+testBundleID1, nil)
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code)
		})
	}
}

func TestBrowseCalendarDayWithoutMonth(t *testing.T) {
	// A day given alone pulls its own month into view.
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	req := httptest.NewRequest(http.MethodGet, "/browse?day=2026-03-10", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "March 2026")
	// The selected cell is highlighted even though the day has no bundle.
	require.Contains(t, body, `class="table-primary"`)
	require.Contains(t, body, "No bundles on 2026-03-10.")
}

func TestBrowseCalendarGarbageMonthFallsBack(t *testing.T) {
	// An unparseable month silently falls back to the current one.
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	cookie := loginCookie(t, h)
	req := httptest.NewRequest(http.MethodGet, "/browse?month=garbage", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), time.Now().Format("January 2006"))
}
