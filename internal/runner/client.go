// Package runner implements octopus-runner's side of the Phase 7
// protocol: a persistent outbound WebSocket connection to the server, over
// which it receives GitJobs and executes them using the exact same
// tools.GitRunner / tools.CLIRunner code Phase 6 runs directly on the
// server — swapping LocalDispatcher for a remote runner never touches
// that execution code, only how a job arrives.
package runner

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"octopus/internal/domain"
	"octopus/internal/tools"
)

type wireMessage struct {
	Type   string               `json:"type"`
	Job    *domain.GitJob       `json:"job,omitempty"`
	Result *domain.GitJobResult `json:"result,omitempty"`
}

// Client is octopus-runner's connection to the central server.
type Client struct {
	ServerURL      string // ws://host:port/runner/connect or wss://...
	Token          string
	Dispatcher     *tools.LocalDispatcher
	Queue          *LocalQueue
	ReconnectDelay time.Duration // defaults to 3s if zero

	mu   sync.Mutex
	conn *websocket.Conn
}

// Run connects, serves jobs, and reconnects with a fixed delay on any
// disconnect, until ctx is cancelled. It's meant to be the entire body of
// cmd/octopus-runner/main.go's run loop — a dropped connection is an
// ordinary event here, not a fatal error (PLAN.md Key Design Decision 15
// is about the server's view of this; symmetrically, the runner just
// keeps trying rather than exiting).
func (c *Client) Run(ctx context.Context) error {
	delay := c.ReconnectDelay
	if delay == 0 {
		delay = 3 * time.Second
	}

	for {
		err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Error("runner: connection lost, will retry", "error", err, "retry_in", delay)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (c *Client) connectAndServe(ctx context.Context) error {
	header := http.Header{}
	header.Set("X-Runner-Token", c.Token)

	ws, _, err := websocket.DefaultDialer.DialContext(ctx, c.ServerURL, header)
	if err != nil {
		return err
	}
	defer ws.Close()

	c.mu.Lock()
	c.conn = ws
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
	}()

	slog.Info("runner: connected", "server", c.ServerURL)

	// ReadJSON below blocks on the raw connection and doesn't watch ctx by
	// itself — without this, cancelling ctx (process shutdown, or a test
	// simulating this runner going offline) would leave the connection
	// open until the *server* noticed. Closing it here is what turns
	// "context cancelled" into an actual disconnect.
	stopWatcher := make(chan struct{})
	defer close(stopWatcher)
	go func() {
		select {
		case <-ctx.Done():
			ws.Close()
		case <-stopWatcher:
		}
	}()

	// Flush anything left over from a previous disconnect (or a prior
	// process that crashed with results still queued) before accepting
	// new work.
	c.flushUnsentResults()

	for {
		var msg wireMessage
		if err := ws.ReadJSON(&msg); err != nil {
			return err
		}
		if msg.Type != "job" || msg.Job == nil {
			continue
		}

		// Deliberately not waited on before this function returns: a job
		// in flight when the connection drops keeps running against the
		// Client's own persistent state (Queue, Dispatcher), completely
		// independent of this specific connection attempt — that's the
		// whole point of the local durable queue. Waiting here would only
		// delay reconnection and, worse, let a job finish while c.conn
		// still (briefly) points at the dying connection, racing a result
		// into a socket nobody's reading from anymore instead of queuing
		// it correctly.
		go c.handleJob(ctx, msg.Job)
	}
}

// handleJob executes one job and gets its result durably queued before
// ever attempting to send it — the ordering (save received, execute, save
// result, then try to send) is what makes a disconnect or process
// restart mid-job lossless.
func (c *Client) handleJob(ctx context.Context, job *domain.GitJob) {
	if err := c.Queue.SaveReceived(job); err != nil {
		slog.Error("runner: saving received job locally", "job_id", job.ID, "error", err)
	}

	// LocalDispatcher.Dispatch's error return is always nil by design
	// (job failures land in result.Success/Error, not the error return —
	// see tools/localdispatch.go); the check here is defensive.
	result, err := c.Dispatcher.Dispatch(ctx, job)
	if err != nil {
		slog.Error("runner: dispatch itself failed unexpectedly", "job_id", job.ID, "error", err)
		result = &domain.GitJobResult{JobID: job.ID, Success: false, Error: err.Error()}
	}

	if err := c.Queue.SaveUnsentResult(result); err != nil {
		slog.Error("runner: queuing result locally", "job_id", job.ID, "error", err)
	}

	if err := c.sendResult(result); err == nil {
		_ = c.Queue.MarkSent(result.JobID)
	} else {
		slog.Info("runner: result queued locally, will flush on reconnect", "job_id", job.ID)
	}
}

func (c *Client) sendResult(result *domain.GitJobResult) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	return conn.WriteJSON(wireMessage{Type: "result", Result: result})
}

func (c *Client) flushUnsentResults() {
	results, err := c.Queue.UnsentResults()
	if err != nil {
		slog.Error("runner: loading unsent results from local queue", "error", err)
		return
	}
	for _, r := range results {
		if c.sendResult(r) == nil {
			_ = c.Queue.MarkSent(r.JobID)
		} else {
			// Still not connected somehow — stop, the next reconnect will retry.
			return
		}
	}
}
