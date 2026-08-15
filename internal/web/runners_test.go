package web_test

import (
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"octopus/internal/agents"
	"octopus/internal/agents/echo"
	"octopus/internal/ratelimit"
	"octopus/internal/scheduler"
	"octopus/internal/store"
	"octopus/internal/web"

	"golang.org/x/crypto/bcrypt"
	"net/http/httptest"
)

func TestLogin_RateLimited(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "octopus.db")
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	st, err := store.New(dbPath, "admin", string(hash))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	reg := agents.NewRegistry()
	reg.Register("echo", echo.New)
	sched := scheduler.New(st, reg.Create)

	srv := &web.Server{
		Store: st, Scheduler: sched, Registry: reg, InsecureCookies: true,
		LoginLimiter: ratelimit.New(2, time.Minute),
	}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	post := func() *http.Response {
		resp, err := client.PostForm(ts.URL+"/login", url.Values{"username": {"admin"}, "password": {"wrong"}})
		if err != nil {
			t.Fatalf("POST /login: %v", err)
		}
		return resp
	}

	for i := 0; i < 2; i++ {
		resp := post()
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: expected under the limit, got 429", i+1)
		}
	}

	resp := post()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 429 after exceeding the login rate limit, got %d: %s", resp.StatusCode, body)
	}
}

func TestRunners_CreateListRevoke(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	cookie := login(t, ts)
	do := func(method, path string, body io.Reader) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, body)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.AddCookie(cookie)
		if body != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	resp := do("POST", "/runners", strings.NewReader(url.Values{"name": {"laptop-1"}, "project_ids": {"proj1, proj2"}}.Encode()))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create runner: expected 200, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "octor_") {
		t.Fatalf("expected the raw token to be shown once, got: %s", body)
	}

	resp = do("GET", "/runners", nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "laptop-1") {
		t.Fatalf("expected the new runner listed, got: %s", body)
	}
	if !strings.Contains(string(body), "offline") {
		t.Fatalf("expected the runner to show offline (no Hub wired, never connected), got: %s", body)
	}

	// Extract the runner id from the revoke form's action attribute.
	marker := `action="/runners/`
	i := strings.Index(string(body), marker)
	if i == -1 {
		t.Fatalf("could not find revoke form in runners page: %s", body)
	}
	rest := string(body)[i+len(marker):]
	id := rest[:strings.Index(rest, "/revoke")]

	resp = do("POST", "/runners/"+id+"/revoke", strings.NewReader(""))
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke: expected 303, got %d", resp.StatusCode)
	}

	resp = do("GET", "/runners", nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "laptop-1") {
		t.Fatalf("expected revoked runner to be gone from the list, got: %s", body)
	}
}
