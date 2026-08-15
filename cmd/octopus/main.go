package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"octopus/internal/agents"
	"octopus/internal/agents/echo"
	"octopus/internal/config"
	"octopus/internal/domain"
	"octopus/internal/gateway"
	"octopus/internal/runnerhub"
	"octopus/internal/scheduler"
	"octopus/internal/store"
	"octopus/internal/tools"
	"octopus/internal/web"
)

// Version is overridden at build time via -ldflags "-X main.Version=x.y.z",
// kept in sync with the repo-root VERSION file (see PLAN.md §9). "dev"
// covers plain `go run`/`go build` with no ldflags.
var Version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		runHashPassword(os.Args[2:])
		return
	}

	configPath := os.Getenv("OCTOPUS_CONFIG")
	if configPath == "" {
		// Not "config.yaml": that path is the Home Assistant add-on
		// manifest at the repo root, a completely different file with a
		// completely different schema — loading it here would either
		// fail validation or silently do the wrong thing. Docker and the
		// HA add-on always set OCTOPUS_CONFIG explicitly (see
		// docker-entrypoint.sh), so this default only matters for running
		// straight from source; see octopus.example.yaml.
		configPath = "octopus.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.New(cfg.Store.DSN, cfg.Auth.AdminUsername, cfg.Auth.AdminPasswordHash)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	reg := agents.NewRegistry()
	reg.Register("echo", echo.New)

	cloneCacheDir := cfg.Git.CloneCacheDir
	if cloneCacheDir == "" {
		cloneCacheDir = "./data/clones"
	}

	sched := scheduler.New(st, reg.Create)
	sched.BranchPattern = cfg.Git.BranchPattern

	var hub *runnerhub.Hub
	var dispatcher domain.JobDispatcher
	if cfg.Runner.Enabled {
		hub = runnerhub.New(st)
		// The moment a runner connects, retry every run that's been
		// sitting AWAITING_RUNNER for one of the projects it serves —
		// otherwise "resumes automatically" would require a human to
		// notice and retry (PLAN.md Phase 7).
		hub.OnRunnerConnected = func(projectIDs []string) {
			resumeAwaitingRunner(context.Background(), sched, st, projectIDs)
		}
		dispatcher = hub
		slog.Info("runner protocol enabled", "endpoint", "/runner/connect")
	} else {
		dispatcher = tools.NewLocalDispatcher(cloneCacheDir)
	}
	sched.Dispatcher = dispatcher

	agents.RegisterCLIPresets(reg, agents.PresetConfig{
		Dispatcher:        dispatcher,
		Store:             st,
		AnthropicAPIKey:   cfg.Agents.AnthropicAPIKey,
		OpenAIAPIKey:      cfg.Agents.OpenAIAPIKey,
		AntigravityAPIKey: cfg.Agents.AntigravityAPIKey,
	})

	if err := sched.ResumeActive(context.Background()); err != nil {
		log.Fatalf("resuming active runs: %v", err)
	}

	srv := &web.Server{Store: st, Scheduler: sched, Registry: reg}
	if hub != nil {
		srv.Hub = hub
	}
	mux := srv.Routes()

	if hub != nil {
		mux.HandleFunc("GET /runner/connect", hub.HandleConnect)
	}

	if cfg.Slack.SigningSecret != "" {
		webBaseURL := cfg.Web.BaseURL
		if webBaseURL == "" {
			webBaseURL = fmt.Sprintf("http://localhost:%d", cfg.Port)
		}
		gw := gateway.New(st, sched, cfg.Slack.SigningSecret, webBaseURL)
		gw.RegisterRoutes(mux)
		slog.Info("slack gateway enabled")
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	slog.Info("octopus starting", "addr", addr, "version", Version)
	if err := http.ListenAndServe(addr, requestLogger(web.SecurityHeaders(mux))); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// requestLogger gives every request a structured log line (method, path,
// status, duration) — PLAN.md Phase 8's "structured logging ... on every
// line." Handler-level errors (internal/web's serverError, the gateway,
// runnerhub) already attach run_id/project_id where they have them; this
// is the outer layer that covers every request uniformly, including the
// ones that never reach a specific run.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Info("http request",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(), "remote_addr", r.RemoteAddr,
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Hijack lets statusWriter satisfy http.Hijacker by delegating to the
// wrapped ResponseWriter. Without this, requestLogger's wrapping breaks the
// gorilla/websocket upgrade on /runner/connect: Upgrade() type-asserts for
// http.Hijacker, and an embedded http.ResponseWriter interface field doesn't
// promote a Hijack method the interface itself doesn't declare, so every
// runner connection attempt failed with a 500 before reaching the handler.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("statusWriter: underlying ResponseWriter does not support hijacking")
	}
	return hj.Hijack()
}

// resumeAwaitingRunner retries every AWAITING_RUNNER run scoped to
// projectIDs — called right after a runner connects, so a run that's been
// queued waiting for exactly this runner picks back up immediately instead
// of waiting for a human to notice.
func resumeAwaitingRunner(ctx context.Context, sched *scheduler.Scheduler, st *store.SQLiteStore, projectIDs []string) {
	served := make(map[string]bool, len(projectIDs))
	for _, id := range projectIDs {
		served[id] = true
	}

	active, err := st.ListActiveRuns(ctx)
	if err != nil {
		slog.Error("resuming runs after runner connect: listing active runs", "error", err)
		return
	}
	for _, run := range active {
		if run.Status == domain.StatusAwaitingRunner && served[run.ProjectID] {
			sched.ResumeRun(ctx, run.RunID)
		}
	}
}

// runHashPassword implements `octopus hash-password [password]`, used to
// generate AuthConfig.AdminPasswordHash without ever writing a plaintext
// password into the server's own config. If no argument is given it reads
// stdin, so the password doesn't linger in shell history either —
// docker-entrypoint.sh uses exactly that form.
func runHashPassword(args []string) {
	var password string
	if len(args) > 0 {
		password = args[0]
	} else {
		fmt.Fprint(os.Stderr, "Password: ")
		// bufio.Reader.ReadString, not fmt.Scanln: Scanln splits on
		// whitespace, so a passphrase with a space in it ("correct horse
		// battery staple") silently truncates to the first word and then
		// errors on the leftover text ("expected newline") — found by
		// actually testing this against a spaced password, not by
		// inspection.
		input, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			log.Fatalf("reading password: %v", err)
		}
		password = strings.TrimRight(input, "\r\n")
		if password == "" {
			log.Fatal("reading password: empty input")
		}
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("hashing password: %v", err)
	}
	fmt.Println(string(hash))
}
