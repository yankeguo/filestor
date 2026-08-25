package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testCfg() *Config {
	return &Config{Admin: AdminConfig{Username: "admin", Password: "secret"}}
}

func loginCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("username=admin&password=secret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatal("missing session cookie")
	return nil
}

func TestSessionCookieHMAC(t *testing.T) {
	key := sessionCookieKey("admin", "secret")
	exp := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)
	val := signSessionCookie(key, "admin", exp)
	require.True(t, verifySessionCookie(key, "admin", val, exp.Add(-time.Minute)))
	require.False(t, verifySessionCookie(key, "admin", val, exp))
	require.False(t, verifySessionCookie(key, "admin", val+"x", exp.Add(-time.Minute)))
	require.False(t, verifySessionCookie(key, "other", val, exp.Add(-time.Minute)))
	require.False(t, verifySessionCookie(sessionCookieKey("admin", "other"), "admin", val, exp.Add(-time.Minute)))
}

func TestCookieSecureFromForwardedProto(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	require.False(t, cookieSecure(req))
	req.Header.Set("X-Forwarded-Proto", "https")
	require.True(t, cookieSecure(req))
}

func TestLoginAndGuard(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/browse", rec.Header().Get("Location"))

	req = httptest.NewRequest(http.MethodGet, "/browse", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))

	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("username=admin&password=wrong"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, "text/html; charset=utf-8", rec.Result().Header.Get("Content-Type"))
	require.Contains(t, rec.Body.String(), "invalid username or password")
	require.Empty(t, rec.Result().Cookies())

	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("username=admin&password=secret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/browse", rec.Header().Get("Location"))
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	require.NotNil(t, cookie)
	require.True(t, cookie.HttpOnly)
	require.Equal(t, "/", cookie.Path)
	require.False(t, cookie.Secure)

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/browse", rec.Header().Get("Location"))

	req = httptest.NewRequest(http.MethodGet, "/browse", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), ">Mo</th>")
	require.Contains(t, rec.Body.String(), "No bundles on this day.")

	req = httptest.NewRequest(http.MethodGet, "/browse", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value + "tamper"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))

	req = httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))
}

func TestLoginPage(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `name="username"`)
}

func TestLoginRedirectsWhenAlreadySignedIn(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	cookie := loginCookie(t, h)

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/browse", rec.Header().Get("Location"))
}

func TestLogoutClearsCookieWithoutSession(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	require.NotNil(t, cookie)
	require.True(t, cookie.MaxAge < 0)
}

func TestSecurityHeaders(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.Contains(t, rec.Header().Get("Content-Security-Policy"), "cdn.jsdelivr.net")
	require.Contains(t, rec.Header().Get("Content-Security-Policy"), "connect-src 'self'")
	require.Contains(t, rec.Header().Get("Content-Security-Policy"), "font-src https://cdn.jsdelivr.net")
	require.Contains(t, rec.Header().Get("Content-Security-Policy"), "media-src 'self'")
}

func TestHealthzDoesNotRequireLogin(t *testing.T) {
	h := NewServer(testCfg(), &fakeStore{}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "OK", rec.Body.String())
}
