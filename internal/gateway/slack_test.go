package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"octopus/internal/agents"
	"octopus/internal/agents/echo"
	"octopus/internal/domain"
	"octopus/internal/ratelimit"
	"octopus/internal/scheduler"
	"octopus/internal/store"
)

const testSecret = "test-signing-secret"

func sign(t *testing.T, secret, body string, ts time.Time) (string, string) {
	t.Helper()
	tsStr := strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + tsStr + ":" + body))
	return tsStr, "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func signedRequest(t *testing.T, ts *httptest.Server, path, secret, body string, at time.Time) *http.Request {
	t.Helper()
	tsStr, sig := sign(t, secret, body, at)
	req, err := http.NewRequest("POST", ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", tsStr)
	req.Header.Set("X-Slack-Signature", sig)
	return req
}

func newTestGateway(t *testing.T) (*Gateway, *store.SQLiteStore) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "octopus.db")
	st, err := store.New(dbPath, "", "")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	reg := agents.NewRegistry()
	reg.Register("echo", echo.New)
	sched := scheduler.New(st, reg.Create)

	return New(st, sched, testSecret, "http://octopus.example"), st
}

func TestVerifySignature_RejectsBadAndStale(t *testing.T) {
	gw, _ := newTestGateway(t)
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := "text=demo+TICKET-1&response_url=" + url.QueryEscape("http://example.com/response")

	t.Run("valid signature reaches handler", func(t *testing.T) {
		req := signedRequest(t, ts, "/api/slack/command", testSecret, body, time.Now())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
		}
	})

	t.Run("wrong secret is rejected", func(t *testing.T) {
		req := signedRequest(t, ts, "/api/slack/command", "wrong-secret", body, time.Now())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 for wrong secret, got %d", resp.StatusCode)
		}
	})

	t.Run("stale timestamp is rejected", func(t *testing.T) {
		req := signedRequest(t, ts, "/api/slack/command", testSecret, body, time.Now().Add(-10*time.Minute))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 for stale timestamp, got %d", resp.StatusCode)
		}
	})

	t.Run("tampered body is rejected", func(t *testing.T) {
		tsStr, sig := sign(t, testSecret, body, time.Now())
		req, _ := http.NewRequest("POST", ts.URL+"/api/slack/command", strings.NewReader(body+"&extra=1"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Slack-Request-Timestamp", tsStr)
		req.Header.Set("X-Slack-Signature", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 for tampered body, got %d", resp.StatusCode)
		}
	})
}

func TestSlackCommand_RateLimitedPerTeam(t *testing.T) {
	gw, _ := newTestGateway(t)
	gw.CommandLimiter = ratelimit.New(2, time.Minute)
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	bodyFor := func(team string) string {
		return "team_id=" + team + "&text=demo+TICKET-1&response_url=" + url.QueryEscape("http://example.com/response")
	}

	// Team A: two allowed, third blocked.
	for i := 0; i < 2; i++ {
		req := signedRequest(t, ts, "/api/slack/command", testSecret, bodyFor("teamA"), time.Now())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("teamA attempt %d: expected under the limit, got 429", i+1)
		}
	}
	req := signedRequest(t, ts, "/api/slack/command", testSecret, bodyFor("teamA"), time.Now())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected teamA's 3rd command to be rate limited, got %d", resp.StatusCode)
	}

	// Team B has its own independent budget.
	req = signedRequest(t, ts, "/api/slack/command", testSecret, bodyFor("teamB"), time.Now())
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		t.Fatal("expected teamB to have its own independent rate limit budget")
	}
}

// TestSlackCommand_RejectsUnsafeTicketID proves ticket_id is validated
// here too — this is the *least*-trusted place it enters the system (any
// workspace member who can run /octopus, not just the logged-in admin),
// and it flows into a git branch name passed as a bare positional
// argument to checkout/push/merge.
func TestSlackCommand_RejectsUnsafeTicketID(t *testing.T) {
	gw, st := newTestGateway(t)
	ctx := context.Background()
	if err := st.SaveProject(ctx, &domain.Project{ID: "demo", Name: "Demo", GitRemoteURL: "git@x:y.git", BaseBranch: "main", DefaultPipelineDefID: "def1"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, badTicket := range []string{"--force", "-o", "../../etc/passwd"} {
		body := "text=" + url.QueryEscape("demo "+badTicket) + "&response_url=" + url.QueryEscape("http://example.com/response")
		req := signedRequest(t, ts, "/api/slack/command", testSecret, body, time.Now())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do (ticket=%q): %v", badTicket, err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 (Slack wants a 200 even for a rejected command, with an ephemeral error), got %d", resp.StatusCode)
		}
		if !strings.Contains(string(respBody), "may only contain") {
			t.Fatalf("expected an ephemeral rejection message for ticket=%q, got: %s", badTicket, respBody)
		}
	}
}

// TestSlackCommandToReviewToApprove exercises PLAN.md Phase 5's "Done
// when" end to end using a synthetic signed request in place of a real
// Slack workspace: a slash command starts a run, the run pausing at a
// review gate posts a Block Kit card to a fake response_url server, and
// clicking "Approve" (a synthetic block_actions payload) resolves it,
// letting the run finish — each step signed exactly like Slack would sign
// it.
func TestSlackCommandToReviewToApprove(t *testing.T) {
	gw, st := newTestGateway(t)
	ctx := context.Background()

	if err := st.SaveProject(ctx, &domain.Project{ID: "demo", Name: "Demo", GitRemoteURL: "git@x:y.git", BaseBranch: "main"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	def := &domain.PipelineDef{
		ID: "def1", ProjectID: "demo", Name: "def1",
		Nodes: []domain.NodeDef{
			{ID: "A", AgentType: "echo", Config: map[string]any{"output": "a-out"}},
			{ID: "B", AgentType: "echo", Config: map[string]any{"output": "b-out"}, RequiresReview: true},
		},
		Edges: []domain.EdgeDef{{From: "A", To: "B"}},
	}
	if err := st.SavePipelineDef(ctx, def); err != nil {
		t.Fatalf("SavePipelineDef: %v", err)
	}
	if err := st.SaveProject(ctx, &domain.Project{ID: "demo", Name: "Demo", GitRemoteURL: "git@x:y.git", BaseBranch: "main", DefaultPipelineDefID: "def1"}); err != nil {
		t.Fatalf("setting default pipeline: %v", err)
	}

	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	slackSrv := httptest.NewServer(mux)
	defer slackSrv.Close()

	// Fake Slack's response_url endpoint — captures whatever Octopus posts.
	var mu sync.Mutex
	var posted []slackMessage
	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg slackMessage
		_ = json.NewDecoder(r.Body).Decode(&msg)
		mu.Lock()
		posted = append(posted, msg)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeSlack.Close()

	cmdBody := url.Values{
		"text":         {"demo TICKET-1"},
		"response_url": {fakeSlack.URL},
	}.Encode()
	req := signedRequest(t, slackSrv, "/api/slack/command", testSecret, cmdBody, time.Now())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("command: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var ack slackMessage
	if err := json.Unmarshal(body, &ack); err != nil {
		t.Fatalf("decoding ack: %v", err)
	}
	if !strings.Contains(ack.Text, "Started run") {
		t.Fatalf("expected ack to mention starting a run, got %q", ack.Text)
	}

	// Wait for the review card to land on the fake Slack endpoint.
	deadline := time.Now().Add(2 * time.Second)
	var reviewMsg slackMessage
	for time.Now().Before(deadline) {
		mu.Lock()
		if len(posted) > 0 {
			reviewMsg = posted[0]
		}
		mu.Unlock()
		if reviewMsg.Text != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reviewMsg.Text == "" {
		t.Fatal("expected a review card to be posted to response_url")
	}
	if !strings.Contains(reviewMsg.Text, "awaiting review") {
		t.Fatalf("expected review card text, got %q", reviewMsg.Text)
	}

	// Extract the actionValue from the "Approve" button's value field.
	blocks := reviewMsg.Blocks
	var approveValue string
	for _, b := range blocks {
		elements, _ := b["elements"].([]any)
		for _, raw := range elements {
			el, _ := raw.(map[string]any)
			if el["action_id"] == "approve" {
				approveValue, _ = el["value"].(string)
			}
		}
	}
	if approveValue == "" {
		t.Fatal("could not find approve button value in review card")
	}

	interaction := map[string]any{
		"type": "block_actions",
		"actions": []map[string]any{
			{"action_id": "approve", "value": approveValue},
		},
		"response_url": fakeSlack.URL,
	}
	payloadJSON, _ := json.Marshal(interaction)
	actionBody := url.Values{"payload": {string(payloadJSON)}}.Encode()

	req = signedRequest(t, slackSrv, "/api/slack/action", testSecret, actionBody, time.Now())
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("action: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("action: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Wait for the final "finished" message.
	deadline = time.Now().Add(2 * time.Second)
	var finalMsg slackMessage
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, m := range posted {
			if strings.Contains(m.Text, "finished") {
				finalMsg = m
			}
		}
		mu.Unlock()
		if finalMsg.Text != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finalMsg.Text == "" {
		t.Fatal("expected a final 'finished' message after approval")
	}
	if !strings.Contains(finalMsg.Text, "COMPLETED") {
		t.Fatalf("expected run to complete, got %q", finalMsg.Text)
	}
}
