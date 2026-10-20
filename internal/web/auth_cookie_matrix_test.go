package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// matrix/hardened: the sign-in cookie must outlive the browser process (a
// session cookie was dropped whenever Android reclaimed Chrome) and renew on
// page loads, but never be minted from a bad cookie or on API calls.
func TestTokenCookieIsLongLivedAndRenewsOnPageLoads(t *testing.T) {
	s := NewServer(Config{ListenAddr: "127.0.0.1:8420", Token: "secret"})
	serve := func(r *http.Request) []*http.Cookie {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w.Result().Cookies()
	}

	got := serve(httptest.NewRequest(http.MethodGet, "http://localhost:8420/?token=secret", nil))
	if len(got) != 1 || got[0].MaxAge != tokenCookieMaxAge || tokenCookieMaxAge < 300*24*60*60 {
		t.Fatalf("tokened visit cookie: %+v", got)
	}

	for _, path := range []string{"/", "/s/sess-1", "/g/work"} {
		r := httptest.NewRequest(http.MethodGet, "http://localhost:8420"+path, nil)
		r.AddCookie(&http.Cookie{Name: "agentdeck_token", Value: "secret"})
		if c := serve(r); len(c) != 1 || c[0].MaxAge != tokenCookieMaxAge || c[0].Value != "secret" {
			t.Errorf("page load %s did not renew the cookie: %+v", path, c)
		}
	}

	noRenew := []*http.Request{
		httptest.NewRequest(http.MethodGet, "http://localhost:8420/api/menu", nil),
		httptest.NewRequest(http.MethodGet, "http://localhost:8420/", nil), // no cookie at all
	}
	noRenew[0].AddCookie(&http.Cookie{Name: "agentdeck_token", Value: "secret"})
	bad := httptest.NewRequest(http.MethodGet, "http://localhost:8420/s/x", nil)
	bad.AddCookie(&http.Cookie{Name: "agentdeck_token", Value: "wrong"})
	noRenew = append(noRenew, bad)
	for _, r := range noRenew {
		if c := serve(r); len(c) != 0 {
			t.Errorf("%s %s (cookie %v) set a cookie: %+v", r.Method, r.URL.Path, r.Cookies(), c)
		}
	}
}
