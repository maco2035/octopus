package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"octopus/internal/domain"
)

// SQLiteStore implements Store on top of modernc.org/sqlite (pure Go, no
// CGO — PLAN.md Key Design Decision 1). All access goes through a single
// pooled connection (SetMaxOpenConns(1)): simplest way to guarantee the
// WAL/busy_timeout pragmas set at open time actually apply to every query,
// since database/sql applies PRAGMAs per-connection, not per-pool.
type SQLiteStore struct {
	db *sql.DB
}

// New opens (creating if needed) a SQLite store at dsn and seeds the v1
// admin account from adminUsername/adminPasswordHash if a username is
// given (PLAN.md Key Design Decision 23). Pass "" for both to skip seeding
// — e.g. for tests that don't exercise login.
func New(dsn string, adminUsername, adminPasswordHash string) (*SQLiteStore, error) {
	if err := ensureDir(dsn); err != nil {
		return nil, fmt.Errorf("preparing sqlite directory: %w", err)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("setting %q: %w", pragma, err)
		}
	}

	s := &SQLiteStore{db: db}

	ctx := context.Background()
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}

	if adminUsername != "" {
		if err := s.seedAdmin(ctx, adminUsername, adminPasswordHash); err != nil {
			db.Close()
			return nil, fmt.Errorf("seeding admin user: %w", err)
		}
	}

	return s, nil
}

func ensureDir(dsn string) error {
	if dsn == ":memory:" || strings.Contains(dsn, ":memory:") {
		return nil
	}
	dir := filepath.Dir(dsn)
	if dir == "." || dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

const schema = `
CREATE TABLE IF NOT EXISTS projects (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	git_remote_url TEXT NOT NULL,
	base_branch TEXT NOT NULL,
	owner TEXT NOT NULL DEFAULT '',
	default_pipeline_def_id TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS pipeline_defs (
	id TEXT PRIMARY KEY,
	project_id TEXT NOT NULL,
	name TEXT NOT NULL,
	nodes_json TEXT NOT NULL,
	edges_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS pipeline_states (
	run_id TEXT PRIMARY KEY,
	project_id TEXT NOT NULL,
	pipeline_def_id TEXT NOT NULL,
	ticket_id TEXT NOT NULL DEFAULT '',
	git_branch TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	pending_node_id TEXT NOT NULL DEFAULT '',
	action_token TEXT NOT NULL DEFAULT '',
	node_outputs_json TEXT NOT NULL DEFAULT '{}',
	summary TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS checkpoints (
	run_id TEXT NOT NULL,
	node_id TEXT NOT NULL,
	PRIMARY KEY (run_id, node_id)
);

CREATE TABLE IF NOT EXISTS runners (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	token_hash TEXT NOT NULL,
	project_ids_json TEXT NOT NULL DEFAULT '[]',
	last_seen TIMESTAMP
);

CREATE TABLE IF NOT EXISTS git_jobs (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL,
	node_id TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL,
	type TEXT NOT NULL,
	payload_json TEXT NOT NULL DEFAULT '{}',
	resolved INTEGER NOT NULL DEFAULT 0,
	success INTEGER,
	output TEXT,
	error TEXT,
	result_session_id TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY,
	username TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
	token TEXT PRIMARY KEY,
	user_id TEXT NOT NULL,
	expires_at TIMESTAMP NOT NULL
);
`

func (s *SQLiteStore) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	// Additive columns for DBs created before these fields existed (both
	// are included in the CREATE TABLEs above for brand-new DBs already).
	for _, stmt := range []string{
		`ALTER TABLE pipeline_states ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN default_pipeline_def_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE git_jobs ADD COLUMN node_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE git_jobs ADD COLUMN result_session_id TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				return fmt.Errorf("running %q: %w", stmt, err)
			}
		}
	}
	return nil
}

func (s *SQLiteStore) seedAdmin(ctx context.Context, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash) VALUES ('admin', ?, ?)
		ON CONFLICT(id) DO UPDATE SET username = excluded.username, password_hash = excluded.password_hash
	`, username, passwordHash)
	return err
}

// ---- Projects ----

func (s *SQLiteStore) SaveProject(ctx context.Context, p *domain.Project) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (id, name, git_remote_url, base_branch, owner, default_pipeline_def_id) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, git_remote_url=excluded.git_remote_url,
			base_branch=excluded.base_branch, owner=excluded.owner, default_pipeline_def_id=excluded.default_pipeline_def_id
	`, p.ID, p.Name, p.GitRemoteURL, p.BaseBranch, p.Owner, p.DefaultPipelineDefID)
	return err
}

func (s *SQLiteStore) LoadProject(ctx context.Context, id string) (*domain.Project, error) {
	p := &domain.Project{ID: id}
	err := s.db.QueryRowContext(ctx, `SELECT name, git_remote_url, base_branch, owner, default_pipeline_def_id FROM projects WHERE id = ?`, id).
		Scan(&p.Name, &p.GitRemoteURL, &p.BaseBranch, &p.Owner, &p.DefaultPipelineDefID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *SQLiteStore) ListProjects(ctx context.Context) ([]*domain.Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, git_remote_url, base_branch, owner, default_pipeline_def_id FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Project
	for rows.Next() {
		p := &domain.Project{}
		if err := rows.Scan(&p.ID, &p.Name, &p.GitRemoteURL, &p.BaseBranch, &p.Owner, &p.DefaultPipelineDefID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- Pipeline defs ----

func (s *SQLiteStore) SavePipelineDef(ctx context.Context, d *domain.PipelineDef) error {
	nodesJSON, err := json.Marshal(d.Nodes)
	if err != nil {
		return err
	}
	edgesJSON, err := json.Marshal(d.Edges)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO pipeline_defs (id, project_id, name, nodes_json, edges_json) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET project_id=excluded.project_id, name=excluded.name,
			nodes_json=excluded.nodes_json, edges_json=excluded.edges_json
	`, d.ID, d.ProjectID, d.Name, string(nodesJSON), string(edgesJSON))
	return err
}

func (s *SQLiteStore) LoadPipelineDef(ctx context.Context, id string) (*domain.PipelineDef, error) {
	d := &domain.PipelineDef{ID: id}
	var nodesJSON, edgesJSON string
	err := s.db.QueryRowContext(ctx, `SELECT project_id, name, nodes_json, edges_json FROM pipeline_defs WHERE id = ?`, id).
		Scan(&d.ProjectID, &d.Name, &nodesJSON, &edgesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(nodesJSON), &d.Nodes); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(edgesJSON), &d.Edges); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *SQLiteStore) ListPipelineDefs(ctx context.Context, projectID string) ([]*domain.PipelineDef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, name, nodes_json, edges_json FROM pipeline_defs WHERE project_id = ? ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.PipelineDef
	for rows.Next() {
		d := &domain.PipelineDef{}
		var nodesJSON, edgesJSON string
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Name, &nodesJSON, &edgesJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(nodesJSON), &d.Nodes); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(edgesJSON), &d.Edges); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---- Pipeline state (runs) ----

func (s *SQLiteStore) Save(ctx context.Context, state *domain.PipelineState) error {
	outputsJSON, err := json.Marshal(state.Snapshot())
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO pipeline_states (run_id, project_id, pipeline_def_id, ticket_id, git_branch, session_id, status, pending_node_id, action_token, node_outputs_json, summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			project_id=excluded.project_id, pipeline_def_id=excluded.pipeline_def_id,
			ticket_id=excluded.ticket_id, git_branch=excluded.git_branch, session_id=excluded.session_id, status=excluded.status,
			pending_node_id=excluded.pending_node_id, action_token=excluded.action_token,
			node_outputs_json=excluded.node_outputs_json, summary=excluded.summary
	`, state.RunID, state.ProjectID, state.PipelineDefID, state.TicketID, state.GitBranch, state.SessionID,
		string(state.Status), state.PendingNodeID, state.ActionToken, string(outputsJSON), state.Summary)
	return err
}

func (s *SQLiteStore) Load(ctx context.Context, runID string) (*domain.PipelineState, error) {
	var projectID, pipelineDefID, ticketID, gitBranch, sessionID, status, pendingNodeID, actionToken, outputsJSON, summary string
	err := s.db.QueryRowContext(ctx, `
		SELECT project_id, pipeline_def_id, ticket_id, git_branch, session_id, status, pending_node_id, action_token, node_outputs_json, summary
		FROM pipeline_states WHERE run_id = ?
	`, runID).Scan(&projectID, &pipelineDefID, &ticketID, &gitBranch, &sessionID, &status, &pendingNodeID, &actionToken, &outputsJSON, &summary)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	outputs := map[string]any{}
	if err := json.Unmarshal([]byte(outputsJSON), &outputs); err != nil {
		return nil, err
	}

	return &domain.PipelineState{
		RunID:         runID,
		ProjectID:     projectID,
		PipelineDefID: pipelineDefID,
		TicketID:      ticketID,
		GitBranch:     gitBranch,
		SessionID:     sessionID,
		Status:        domain.Status(status),
		PendingNodeID: pendingNodeID,
		ActionToken:   actionToken,
		NodeOutputs:   outputs,
		Summary:       summary,
	}, nil
}

var terminalStatuses = []domain.Status{
	domain.StatusCompleted, domain.StatusFailed, domain.StatusRejected, domain.StatusCancelled,
}

func (s *SQLiteStore) ListActiveRuns(ctx context.Context) ([]*domain.PipelineState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, project_id, pipeline_def_id, ticket_id, git_branch, session_id, status, pending_node_id, action_token, node_outputs_json, summary
		FROM pipeline_states WHERE status NOT IN (?, ?, ?, ?) ORDER BY run_id
	`, string(terminalStatuses[0]), string(terminalStatuses[1]), string(terminalStatuses[2]), string(terminalStatuses[3]))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.PipelineState
	for rows.Next() {
		var runID, projectID, pipelineDefID, ticketID, gitBranch, sessionID, status, pendingNodeID, actionToken, outputsJSON, summary string
		if err := rows.Scan(&runID, &projectID, &pipelineDefID, &ticketID, &gitBranch, &sessionID, &status, &pendingNodeID, &actionToken, &outputsJSON, &summary); err != nil {
			return nil, err
		}
		outputs := map[string]any{}
		if err := json.Unmarshal([]byte(outputsJSON), &outputs); err != nil {
			return nil, err
		}
		out = append(out, &domain.PipelineState{
			RunID: runID, ProjectID: projectID, PipelineDefID: pipelineDefID, TicketID: ticketID,
			GitBranch: gitBranch, SessionID: sessionID, Status: domain.Status(status), PendingNodeID: pendingNodeID,
			ActionToken: actionToken, NodeOutputs: outputs, Summary: summary,
		})
	}
	return out, rows.Err()
}

// ---- Checkpoints ----

func (s *SQLiteStore) SaveCheckpoint(ctx context.Context, runID, completedNodeID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO checkpoints (run_id, node_id) VALUES (?, ?)`, runID, completedNodeID)
	return err
}

func (s *SQLiteStore) LoadCheckpoint(ctx context.Context, runID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id FROM checkpoints WHERE run_id = ? ORDER BY node_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var nodeID string
		if err := rows.Scan(&nodeID); err != nil {
			return nil, err
		}
		out = append(out, nodeID)
	}
	return out, rows.Err()
}

// ---- Review resolution ----

func (s *SQLiteStore) ResolveReview(ctx context.Context, runID, actionToken string, approve bool, editedOutputs map[string]any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var status, storedToken, outputsJSON string
	err = tx.QueryRowContext(ctx, `SELECT status, action_token, node_outputs_json FROM pipeline_states WHERE run_id = ?`, runID).
		Scan(&status, &storedToken, &outputsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	// subtle.ConstantTimeCompare, not !=: actionToken is a bearer credential
	// (whoever holds it can approve/reject a review — including, once
	// merge lands, pushing to the base branch), and it arrives over the
	// network on every request (the web form, a Slack button's value). A
	// plain != comparison short-circuits at the first differing byte, so
	// response timing leaks how many leading characters an attacker's
	// guess got right — the exact class of bug this review is looking
	// for. ConstantTimeCompare returns 0 (not a timing leak) when lengths
	// differ, since actionToken's expected length isn't itself secret.
	tokenMatches := subtle.ConstantTimeCompare([]byte(storedToken), []byte(actionToken)) == 1
	if status != string(domain.StatusAwaitingReview) || storedToken == "" || !tokenMatches {
		return ErrStaleActionToken
	}

	newStatus := string(domain.StatusRejected)
	if approve {
		outputs := map[string]any{}
		if err := json.Unmarshal([]byte(outputsJSON), &outputs); err != nil {
			return err
		}
		for k, v := range editedOutputs {
			outputs[k] = v
		}
		b, err := json.Marshal(outputs)
		if err != nil {
			return err
		}
		outputsJSON = string(b)
		newStatus = string(domain.StatusRunning)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE pipeline_states SET status = ?, pending_node_id = '', action_token = '', node_outputs_json = ? WHERE run_id = ?
	`, newStatus, outputsJSON, runID); err != nil {
		return err
	}

	return tx.Commit()
}

// ---- Runners ----

func (s *SQLiteStore) SaveRunner(ctx context.Context, r *domain.Runner) error {
	idsJSON, err := json.Marshal(r.ProjectIDs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO runners (id, name, token_hash, project_ids_json, last_seen) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, token_hash=excluded.token_hash,
			project_ids_json=excluded.project_ids_json, last_seen=excluded.last_seen
	`, r.ID, r.Name, r.TokenHash, string(idsJSON), r.LastSeen)
	return err
}

func (s *SQLiteStore) ListRunners(ctx context.Context) ([]*domain.Runner, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, token_hash, project_ids_json, last_seen FROM runners ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Runner
	for rows.Next() {
		r := &domain.Runner{}
		var idsJSON string
		var lastSeen sql.NullTime
		if err := rows.Scan(&r.ID, &r.Name, &r.TokenHash, &idsJSON, &lastSeen); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(idsJSON), &r.ProjectIDs); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			r.LastSeen = lastSeen.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) TouchRunnerHeartbeat(ctx context.Context, runnerID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE runners SET last_seen = ? WHERE id = ?`, time.Now().UTC(), runnerID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) DeleteRunner(ctx context.Context, runnerID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM runners WHERE id = ?`, runnerID)
	return err
}

// ---- Git jobs ----

func (s *SQLiteStore) SaveGitJob(ctx context.Context, job *domain.GitJob) error {
	payloadJSON, err := json.Marshal(job.Payload)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO git_jobs (id, run_id, node_id, project_id, type, payload_json, resolved) VALUES (?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(id) DO UPDATE SET run_id=excluded.run_id, node_id=excluded.node_id, project_id=excluded.project_id,
			type=excluded.type, payload_json=excluded.payload_json
	`, job.ID, job.RunID, job.NodeID, job.ProjectID, job.Type, string(payloadJSON))
	return err
}

func (s *SQLiteStore) LoadPendingGitJobs(ctx context.Context) ([]*domain.GitJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, node_id, project_id, type, payload_json FROM git_jobs WHERE resolved = 0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.GitJob
	for rows.Next() {
		j := &domain.GitJob{}
		var payloadJSON string
		if err := rows.Scan(&j.ID, &j.RunID, &j.NodeID, &j.ProjectID, &j.Type, &payloadJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payloadJSON), &j.Payload); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) LoadPendingGitJobFor(ctx context.Context, runID, nodeID string) (*domain.GitJob, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, node_id, project_id, type, payload_json FROM git_jobs
		WHERE run_id = ? AND node_id = ? AND resolved = 0 ORDER BY id LIMIT 1
	`, runID, nodeID)

	j := &domain.GitJob{}
	var payloadJSON string
	err := row.Scan(&j.ID, &j.RunID, &j.NodeID, &j.ProjectID, &j.Type, &payloadJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(payloadJSON), &j.Payload); err != nil {
		return nil, err
	}
	return j, nil
}

func (s *SQLiteStore) ResolveGitJob(ctx context.Context, jobID string, result *domain.GitJobResult) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE git_jobs SET resolved = 1, success = ?, output = ?, error = ?, result_session_id = ? WHERE id = ?
	`, result.Success, result.Output, result.Error, result.SessionID, jobID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) LoadGitJobResult(ctx context.Context, jobID string) (*domain.GitJobResult, error) {
	var resolved bool
	var success sql.NullBool
	var output, jobErr, sessionID sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT resolved, success, output, error, result_session_id FROM git_jobs WHERE id = ?
	`, jobID).Scan(&resolved, &success, &output, &jobErr, &sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !resolved {
		return nil, ErrNotFound
	}
	return &domain.GitJobResult{
		JobID: jobID, Success: success.Bool, Output: output.String, Error: jobErr.String, SessionID: sessionID.String,
	}, nil
}

// ---- Users & sessions ----

func (s *SQLiteStore) LoadUserByUsername(ctx context.Context, username string) (*domain.User, error) {
	u := &domain.User{Username: username}
	err := s.db.QueryRowContext(ctx, `SELECT id, password_hash FROM users WHERE username = ?`, username).Scan(&u.ID, &u.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s *SQLiteStore) LoadUserByID(ctx context.Context, id string) (*domain.User, error) {
	u := &domain.User{ID: id}
	err := s.db.QueryRowContext(ctx, `SELECT username, password_hash FROM users WHERE id = ?`, id).Scan(&u.Username, &u.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s *SQLiteStore) SaveSession(ctx context.Context, sess *domain.Session) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)
		ON CONFLICT(token) DO UPDATE SET user_id=excluded.user_id, expires_at=excluded.expires_at
	`, sess.Token, sess.UserID, sess.ExpiresAt)
	return err
}

func (s *SQLiteStore) LoadSession(ctx context.Context, token string) (*domain.Session, error) {
	sess := &domain.Session{Token: token}
	err := s.db.QueryRowContext(ctx, `SELECT user_id, expires_at FROM sessions WHERE token = ?`, token).Scan(&sess.UserID, &sess.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *SQLiteStore) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token)
	return err
}
