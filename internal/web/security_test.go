package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"octopus/internal/domain"
)

// TestCreateProject_RejectsUnsafeID proves the server re-validates
// Project.ID itself rather than trusting the HTML form's client-side
// pattern="[a-z0-9-]+" attribute — anyone can bypass that by POSTing
// directly, and Project.ID becomes a filesystem path segment under
// clone_cache_dir (tools.GitRunner.WorkDir).
func TestCreateProject_RejectsUnsafeID(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	cookie := login(t, ts)
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}

	for _, badID := range []string{"../../etc/passwd", "../secret", "a/b", "-leading-dash", ""} {
		req, _ := http.NewRequest("POST", ts.URL+"/projects", strings.NewReader(url.Values{
			"id": {badID}, "name": {"x"}, "git_remote_url": {"git@x:y.git"}, "base_branch": {"main"},
		}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /projects (id=%q): %v", badID, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected id=%q to be rejected with 400, got %d", badID, resp.StatusCode)
		}
	}
}

// TestTriggerRun_RejectsUnsafeTicketID proves ticket_id is validated
// server-side before it can reach a git command line as a branch name
// (tools.GitRunner passes it as a bare positional argument to
// checkout/push/merge).
func TestTriggerRun_RejectsUnsafeTicketID(t *testing.T) {
	srv, st := newTestServer(t)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	ctx := context.Background()
	if err := st.SaveProject(ctx, &domain.Project{ID: "proj1", Name: "Proj1", GitRemoteURL: "git@x:y.git", BaseBranch: "main"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	if err := st.SavePipelineDef(ctx, &domain.PipelineDef{ID: "def1", ProjectID: "proj1", Name: "def1"}); err != nil {
		t.Fatalf("SavePipelineDef: %v", err)
	}

	cookie := login(t, ts)
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}

	for _, badTicket := range []string{"--force", "-o", "../../etc/passwd", "a b"} {
		req, _ := http.NewRequest("POST", ts.URL+"/projects/proj1/pipelines/def1/run", strings.NewReader(url.Values{"ticket_id": {badTicket}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST run (ticket_id=%q): %v", badTicket, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected ticket_id=%q to be rejected with 400, got %d: %s", badTicket, resp.StatusCode, body)
		}
	}
}

// TestTriggerRun_AcceptsSafeTicketID is the companion positive case —
// proves the validation isn't so strict it breaks ordinary use.
func TestTriggerRun_AcceptsSafeTicketID(t *testing.T) {
	srv, st := newTestServer(t)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	ctx := context.Background()
	if err := st.SaveProject(ctx, &domain.Project{ID: "proj1", Name: "Proj1", GitRemoteURL: "git@x:y.git", BaseBranch: "main"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	if err := st.SavePipelineDef(ctx, &domain.PipelineDef{
		ID: "def1", ProjectID: "proj1", Name: "def1",
		Nodes: []domain.NodeDef{{ID: "only", AgentType: "echo"}},
	}); err != nil {
		t.Fatalf("SavePipelineDef: %v", err)
	}

	cookie := login(t, ts)
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}

	req, _ := http.NewRequest("POST", ts.URL+"/projects/proj1/pipelines/def1/run", strings.NewReader(url.Values{"ticket_id": {"JIRA-1234"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected a normal ticket_id to be accepted (303), got %d", resp.StatusCode)
	}
}
