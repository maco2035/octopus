package web_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"octopus/internal/web"
)

func TestSecurityHeaders_SetOnEveryResponse(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(web.SecurityHeaders(srv.Routes()))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	cases := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "SAMEORIGIN",
		"Referrer-Policy":        "same-origin",
	}
	for header, want := range cases {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("header %s: got %q, want %q", header, got, want)
		}
	}
}
