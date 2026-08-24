package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookieName = "filestor"
	sessionCookieTTL  = 12 * time.Hour
)

func sessionCookieKey(username, password string) []byte {
	sum := sha256.Sum256([]byte("filestor\n" + username + "\n" + password))
	return sum[:]
}

func constEqSHA256(a, b string) bool {
	aa := sha256.Sum256([]byte(a))
	bb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(aa[:], bb[:]) == 1
}

func signSessionCookie(key []byte, username string, exp time.Time) string {
	expStr := strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(username + "|" + expStr))
	return expStr + "|" + hex.EncodeToString(mac.Sum(nil))
}

func verifySessionCookie(key []byte, username, value string, now time.Time) bool {
	expStr, macHex, ok := strings.Cut(value, "|")
	if !ok || expStr == "" || macHex == "" {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || now.Unix() >= exp {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(username + "|" + expStr))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(macHex)
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

func cookieSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request) {
	exp := time.Now().UTC().Add(sessionCookieTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    signSessionCookie(s.sessionKey, s.Config.Admin.Username, exp),
		Path:     "/",
		MaxAge:   int(sessionCookieTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(r),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(r),
	})
}

func (s *Server) sessionCookieValid(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" || s.Config == nil {
		return false
	}
	return verifySessionCookie(s.sessionKey, s.Config.Admin.Username, c.Value, time.Now().UTC())
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.sessionCookieValid(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) checkLogin(user, pass string) bool {
	if s.Config == nil {
		return false
	}
	userOK := constEqSHA256(user, s.Config.Admin.Username)
	passOK := constEqSHA256(pass, s.Config.Admin.Password)
	return userOK && passOK
}
