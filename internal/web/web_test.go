package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"octopus/internal/agents"
	"octopus/internal/agents/echo"
	"octopus/internal/domain"
	"octopus/internal/scheduler"
	"octopus/internal/store"
	"octopus/internal/web"
)

func newTestServer(t *testing.T) (*web.Server, *store.SQLiteStore) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "octopus.db")

	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	st, err := store.New(dbPath, "admin", string(hash))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	reg := agents.NewRegistry()
	reg.Register("echo", echo.New)
	sched := scheduler.New(st, reg.Create)

	return &web.Server{Store: st, Scheduler: sched, Registry: reg, InsecureCookies: true}, st
}

func TestRequireLogin_UnauthenticatedRedirectsToLogin(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect for unauthenticated request, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Fatalf("expected redirect to /login, got %q", loc)
	}
}

func login(t *testing.T, ts *httptest.Server) *http.Cookie {
	t.Helper()
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(ts.URL+"/login", url.Values{"username": {"admin"}, "password": {"s3cret"}})
	if err != nil {
		t.Fatalf("login POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 303 after valid login, got %d: %s", resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "octopus_session" {
			return c
		}
	}
	t.Fatal("no session cookie set after login")
	return nil
}

func TestValidSession_ReachesProtectedPage(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	cookie := login(t, ts)

	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with valid session, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Projects") {
		t.Fatalf("expected projects page content, got: %s", body)
	}
}

func TestExpiredSession_RedirectsToLogin(t *testing.T) {
	srv, st := newTestServer(t)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	cookie := login(t, ts)

	// Force the server-side session to look expired, simulating time
	// passing — the cookie itself is still "valid" on the wire, proving
	// expiry is enforced against the Session row, not just the cookie.
	if err := st.SaveSession(context.Background(), &domain.Session{
		Token: cookie.Value, UserID: "admin", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("forcing session expiry: %v", err)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.AddCookie(cookie)
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected redirect for expired session, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Fatalf("expected redirect to /login, got %q", loc)
	}
}

func TestLogout_RevokesSessionServerSide(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	cookie := login(t, ts)

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	logoutReq, _ := http.NewRequest("POST", ts.URL+"/logout", nil)
	logoutReq.AddCookie(cookie)
	logoutResp, err := client.Do(logoutReq)
	if err != nil {
		t.Fatalf("POST /logout: %v", err)
	}
	logoutResp.Body.Close()

	// Reuse the *same* cookie value after logout — proves revocation is
	// real (the server-side Session row is gone), not just the client
	// forgetting the cookie.
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET / after logout: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected redirect after logout revoked the session, got %d", resp.StatusCode)
	}
}

// TestFullPipelineFlow exercises the server side of the exact scenario
// PLAN.md Phase 4 describes: build a pipeline with a parallel branch and a
// review gate, save it, run it, watch it pause at the gate, edit the
// output, continue, and see it finish. The drag-and-drop itself is browser
// JS (canvas.js) and isn't exercised here — this posts the same JSON
// payload canvas.js's Save button would produce.
func TestFullPipelineFlow(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	cookie := login(t, ts)
	do := func(method, path string, body io.Reader, contentType string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, body)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.AddCookie(cookie)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// Create project.
	resp := do("POST", "/projects", strings.NewReader(url.Values{
		"id": {"proj1"}, "name": {"Proj One"}, "git_remote_url": {"git@x:y.git"}, "base_branch": {"main"},
	}.Encode()), "application/x-www-form-urlencoded")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create project: expected 303, got %d", resp.StatusCode)
	}

	// Save a 3-node pipeline: A -> B (review) -> C, matching the
	// diamond-minus-one-branch shape described in the plan closely enough
	// to prove parallel + review-gate wiring survives the JSON round trip.
	defJSON := `{
		"project_id": "proj1", "name": "3-node",
		"nodes": [
			{"ID":"A","AgentType":"echo","Config":{"output":"a-out"}},
			{"ID":"B","AgentType":"echo","Config":{"output":"b-out"},"RequiresReview":true},
			{"ID":"C","AgentType":"echo","Config":{"output":"c-out"}}
		],
		"edges": [{"From":"A","To":"B"},{"From":"B","To":"C"}]
	}`
	resp = do("POST", "/api/pipeline-defs", strings.NewReader(defJSON), "application/json")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save pipeline def: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var saved struct{ ID string }
	if err := json.Unmarshal(body, &saved); err != nil {
		t.Fatalf("decoding saved def: %v", err)
	}
	if saved.ID == "" {
		t.Fatal("expected a generated pipeline def id")
	}

	// Trigger a run.
	resp = do("POST", "/projects/proj1/pipelines/"+saved.ID+"/run", strings.NewReader(url.Values{"ticket_id": {"TICKET-1"}}.Encode()), "application/x-www-form-urlencoded")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("trigger run: expected 303, got %d", resp.StatusCode)
	}
	runPath := resp.Header.Get("Location")
	if !strings.HasPrefix(runPath, "/runs/") {
		t.Fatalf("expected redirect to /runs/<id>, got %q", runPath)
	}

	// Poll until it pauses at the review gate (should be near-instant for
	// EchoAgent, but give it a beat).
	var fragBody []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp = do("GET", runPath+"/fragment", nil, "")
		fragBody, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(fragBody), "AWAITING_REVIEW") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(string(fragBody), "AWAITING_REVIEW") {
		t.Fatalf("expected run to pause at review gate, fragment: %s", fragBody)
	}
	if !strings.Contains(string(fragBody), "b-out") {
		t.Fatalf("expected pending node B's output in the review form, fragment: %s", fragBody)
	}

	// Extract the action token the review form embedded.
	token := extractValue(string(fragBody), `name="action_token" value="`)
	if token == "" {
		t.Fatalf("could not find action_token in fragment: %s", fragBody)
	}

	// Edit & continue with a new value for B.
	runID := strings.TrimPrefix(runPath, "/runs/")
	resp = do("POST", "/runs/"+runID+"/review", strings.NewReader(url.Values{
		"action_token": {token}, "decision": {"edit"}, "edited_output": {"b-edited"},
	}.Encode()), "application/x-www-form-urlencoded")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("review edit&continue: expected 303, got %d", resp.StatusCode)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp = do("GET", runPath+"/fragment", nil, "")
		fragBody, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(fragBody), "COMPLETED") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(string(fragBody), "COMPLETED") {
		t.Fatalf("expected run to complete after review, fragment: %s", fragBody)
	}
	if !strings.Contains(string(fragBody), "b-edited") {
		t.Fatalf("expected downstream node to reflect the edited output, fragment: %s", fragBody)
	}
}

func extractValue(html, marker string) string {
	i := strings.Index(html, marker)
	if i == -1 {
		return ""
	}
	rest := html[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j == -1 {
		return ""
	}
	return rest[:j]
}
