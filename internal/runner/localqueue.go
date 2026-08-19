package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"octopus/internal/domain"
)

// LocalQueue is octopus-runner's own durable local state (PLAN.md Phase 7:
// "Runners keep a small local durable queue"), completely separate from
// the server's database. Every received job is written here before
// execution starts, and every result is written here before the runner
// even attempts to send it — so a connection drop mid-job, or the runner
// process itself restarting mid-job, never loses a result: on reconnect,
// UnsentResults() is what gets flushed first, before any new job is
// accepted.
type LocalQueue struct {
	db *sql.DB
}

func NewLocalQueue(path string) (*LocalQueue, error) {
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("preparing local queue directory: %w", err)
			}
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening local queue db: %w", err)
	}
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{"PRAGMA journal_mode = WAL", "PRAGMA busy_timeout = 5000"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}

	const schema = `
	CREATE TABLE IF NOT EXISTS received_jobs (
		id TEXT PRIMARY KEY,
		job_json TEXT NOT NULL,
		received_at TIMESTAMP NOT NULL
	);
	CREATE TABLE IF NOT EXISTS unsent_results (
		job_id TEXT PRIMARY KEY,
		result_json TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL
	);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating local queue db: %w", err)
	}

	return &LocalQueue{db: db}, nil
}

func (q *LocalQueue) Close() error { return q.db.Close() }

// SaveReceived records a job before execution starts, so a crash mid-job
// leaves a trace of what this runner was working on.
func (q *LocalQueue) SaveReceived(job *domain.GitJob) error {
	// The live job may contain a provider credential used by the subprocess.
	// The durable recovery record must never contain that credential.
	b, err := json.Marshal(job.Redacted())
	if err != nil {
		return err
	}
	_, err = q.db.ExecContext(context.Background(), `
		INSERT INTO received_jobs (id, job_json, received_at) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET job_json = excluded.job_json
	`, job.ID, string(b), time.Now().UTC())
	return err
}

// SaveUnsentResult records a finished result before the runner even
// attempts to send it — the core of the durability guarantee: execution
// finishing and the result being safely queued locally happen before any
// network I/O.
func (q *LocalQueue) SaveUnsentResult(result *domain.GitJobResult) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = q.db.ExecContext(context.Background(), `
		INSERT INTO unsent_results (job_id, result_json, created_at) VALUES (?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET result_json = excluded.result_json
	`, result.JobID, string(b), time.Now().UTC())
	return err
}

// MarkSent removes a result from the local queue once the server has it —
// safe to call even if the row is already gone (idempotent).
func (q *LocalQueue) MarkSent(jobID string) error {
	_, err := q.db.ExecContext(context.Background(), `DELETE FROM unsent_results WHERE job_id = ?`, jobID)
	return err
}

// UnsentResults returns everything still queued — what reconnect logic
// flushes before resuming normal dispatch.
func (q *LocalQueue) UnsentResults() ([]*domain.GitJobResult, error) {
	rows, err := q.db.QueryContext(context.Background(), `SELECT result_json FROM unsent_results ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.GitJobResult
	for rows.Next() {
		var resultJSON string
		if err := rows.Scan(&resultJSON); err != nil {
			return nil, err
		}
		var result domain.GitJobResult
		if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
			return nil, err
		}
		out = append(out, &result)
	}
	return out, rows.Err()
}
