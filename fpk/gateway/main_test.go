package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTokenAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := tokenAuth("secret-token", next)

	cases := []struct {
		name   string
		path   string
		cookie string
		header string
		want   int
	}{
		{"health is open", "/__imi/health", "", "", http.StatusNoContent},
		{"old health is open", "/health", "", "", http.StatusNoContent},
		{"api needs token", "/api/streams", "", "", http.StatusForbidden},
		{"cookie is accepted", "/api/streams", "secret-token", "", http.StatusNoContent},
		{"header is accepted", "/api/streams", "", "secret-token", http.StatusNoContent},
		{"wrong token is rejected", "/api/streams", "nope", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "imi_app", Value: tc.cookie})
			}
			if tc.header != "" {
				req.Header.Set("X-IMI-Token", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s: got %d, want %d", tc.path, rec.Code, tc.want)
			}
		})
	}
}

// 安卓版 go2rtc 开了 local_auth：网关反代时必须带上账号密码。
func TestProxyInjectsBasicAuth(t *testing.T) {
	cases := []struct {
		name, user, pass, want string
	}{
		{"with creds", "imi", "s3cret", "Basic " + base64.StdEncoding.EncodeToString([]byte("imi:s3cret"))},
		{"without creds", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Authorization")
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()

			proxy := newProxy(upstream.URL, tc.user, tc.pass)
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api", nil))
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d", rec.Code)
			}
			if got != tc.want {
				t.Fatalf("Authorization = %q, want %q", got, tc.want)
			}
		})
	}
}
