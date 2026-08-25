package main

import (
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
	lastList [2]string
	lastKey  string
	listFn   func(prefix, marker string) (ListPage, error)

	putErr   error
	putHook  func(key string)
	putBlock chan struct{}
	putsMu   sync.Mutex
	puts     []putCall
}

func (f *fakeStore) List(prefix, marker string) (ListPage, error) {
	f.lastList = [2]string{prefix, marker}
	if f.listFn != nil {
		return f.listFn(prefix, marker)
	}
	return f.page, f.err
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
	store := &fakeStore{page: ListPage{
		Prefixes: []string{
			"2026/08/202608240659-weekly-report/",
			"2026/08/202608241200-monthly-billing/",
			"2026/08/202608101015-发票/",
			"2026/08/legacy-dir/",
		},
	}}
	h := NewServer(testCfg(), store).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/browse?month=2026-08&day=2026-08-24", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "2026/08/", store.lastList[0])
	body := rec.Body.String()
	require.Contains(t, body, "August 2026")
	// Days with records link back to the day view.
	require.Contains(t, body, `/browse?month=2026-08&day=2026-08-24`)
	require.Contains(t, body, `day=2026-08-10`)
	// Non-conforming directories are not counted.
	require.NotContains(t, body, "legacy-dir")
	// The selected day lists its directories with parsed time and title.
	require.Contains(t, body, "06:59")
	require.Contains(t, body, "weekly-report")
	require.Contains(t, body, "12:00")
	require.Contains(t, body, "monthly-billing")
	require.Contains(t, body, `/browse?prefix=2026%2f08%2f202608240659-weekly-report%2f`)
	// Directories from other days are not listed in the day pane.
	require.NotContains(t, body, "10:15")
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
	require.Contains(t, body, "Select a day")
	require.NotContains(t, body, "2026-08-24")
}

func TestBrowseCalendarFollowsPagination(t *testing.T) {
	store := &fakeStore{listFn: func(prefix, marker string) (ListPage, error) {
		require.Equal(t, "2026/08/", prefix)
		if marker == "" {
			return ListPage{
				Prefixes:    []string{"2026/08/202608011200-a/"},
				IsTruncated: true,
				NextMarker:  "2026/08/202608011200-a/",
			}, nil
		}
		require.Equal(t, "2026/08/202608011200-a/", marker)
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
	d := buildBrowseCalendar(month, now, []string{"2026/08/202608240659-x/"}, now)
	require.Equal(t, "2026-08", d.Month)
	require.Equal(t, "2026-07", d.PrevMonth)
	require.Equal(t, "2026-09", d.NextMonth)
	// Monday-first grid: the 1st (Saturday) sits in column 5.
	require.Equal(t, 1, d.Weeks[0][5].Day)
	require.Equal(t, 0, d.Weeks[0][4].Day)
	// The 31st lands in the first column of the last week.
	last := d.Weeks[len(d.Weeks)-1]
	require.Equal(t, 31, last[0].Day)
	// The selected day is flagged, highlighted with its record, and listed.
	// (2026-08-24 is the Monday of the fifth row.)
	sel := d.Weeks[4][0]
	require.Equal(t, 24, sel.Day)
	require.True(t, sel.Selected)
	require.True(t, sel.Today)
	require.Equal(t, 1, sel.Records)
	require.Equal(t, []calDir{{Time: "06:59", Title: "x", Prefix: "2026/08/202608240659-x/"}}, d.DayDirs)
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
	require.Equal(t, "docs/", store.lastList[0])
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
