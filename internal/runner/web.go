package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

type WebServer struct {
	manager *Manager
	server  *http.Server
	port    int
}

func NewWebServer(m *Manager, port int) *WebServer {
	if port == 0 {
		port = m.GetWebPort()
	}
	return &WebServer{
		manager: m,
		port:    port,
	}
}

type StatusResponse struct {
	Status        string       `json:"status"`
	LastError     string       `json:"last_error"`
	LastConnected string       `json:"last_connected,omitempty"`
	ConfigPath    string       `json:"config_path"`
	Config        Config       `json:"config"`
	ActiveJob     *JobRecord   `json:"active_job,omitempty"`
	History       []*JobRecord `json:"history"`
	Tools         []ToolInfo   `json:"tools"`
}

func (ws *WebServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", ws.handleIndex)
	mux.HandleFunc("GET /api/status", ws.handleAPIStatus)
	mux.HandleFunc("POST /api/config", ws.handleAPISaveConfig)
	mux.HandleFunc("POST /api/reconnect", ws.handleAPIReconnect)

	addr := fmt.Sprintf("0.0.0.0:%d", ws.port)
	ws.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("binding runner web port %d: %w", ws.port, err)
	}

	slog.Info("runner web UI started", "url", fmt.Sprintf("http://localhost:%d", ws.port))

	go func() {
		if err := ws.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("runner web UI server error", "error", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ws.server.Shutdown(shutdownCtx)
	}()

	return nil
}

func (ws *WebServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t, err := template.New("index").Parse(runnerHTMLTemplate)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = t.Execute(w, nil)
}

func (ws *WebServer) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	status, lastErr, lastConn, activeJob, _ := ws.manager.GetStatus()
	cfg := ws.manager.GetConfig()

	// Mask token partially for security in status response
	maskedToken := cfg.RunnerToken
	if len(maskedToken) > 8 {
		maskedToken = maskedToken[:4] + "••••••••" + maskedToken[len(maskedToken)-4:]
	}

	var lastConnStr string
	if lastConn != nil {
		lastConnStr = lastConn.Format(time.RFC3339)
	}

	resp := StatusResponse{
		Status:        string(status),
		LastError:     lastErr,
		LastConnected: lastConnStr,
		ConfigPath:    ws.manager.ConfigPath(),
		Config: Config{
			ServerURL:     cfg.ServerURL,
			RunnerToken:   cfg.RunnerToken,
			ProjectIDs:    cfg.ProjectIDs,
			CloneCacheDir: cfg.CloneCacheDir,
			LocalQueueDB:  cfg.LocalQueueDB,
			WebPort:       cfg.WebPort,
		},
		ActiveJob: activeJob,
		History:   ws.manager.GetHistory(),
		Tools:     ws.manager.CheckTools(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (ws *WebServer) handleAPISaveConfig(w http.ResponseWriter, r *http.Request) {
	var req Config
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	req.ServerURL = strings.TrimSpace(req.ServerURL)
	req.RunnerToken = strings.TrimSpace(req.RunnerToken)
	req.CloneCacheDir = strings.TrimSpace(req.CloneCacheDir)
	req.LocalQueueDB = strings.TrimSpace(req.LocalQueueDB)

	if err := ws.manager.SaveConfig(req); err != nil {
		http.Error(w, fmt.Sprintf("saving config: %v", err), http.StatusInternalServerError)
		return
	}

	ws.manager.Restart(r.Context())

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": "Configuration saved and runner reconnecting"})
}

func (ws *WebServer) handleAPIReconnect(w http.ResponseWriter, r *http.Request) {
	ws.manager.Restart(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": "Reconnecting"})
}

const runnerHTMLTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Octopus Runner Dashboard</title>
  <style>
    :root {
      --bg: #0b0f19;
      --card-bg: #131b2e;
      --card-border: #1e293b;
      --text: #f8fafc;
      --text-muted: #94a3b8;
      --accent: #38bdf8;
      --accent-hover: #0284c7;
      --success: #10b981;
      --success-bg: rgba(16, 185, 129, 0.1);
      --warning: #f59e0b;
      --warning-bg: rgba(245, 158, 11, 0.1);
      --danger: #f43f5e;
      --danger-bg: rgba(244, 63, 94, 0.1);
      --font: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Oxygen, Ubuntu, Cantarell, sans-serif;
      --mono: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
    }

    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      background: var(--bg);
      color: var(--text);
      font-family: var(--font);
      line-height: 1.5;
      padding: 2rem 1rem;
    }
    .container {
      max-width: 960px;
      margin: 0 auto;
    }
    header {
      display: flex;
      justify-content: space-between;
      align-items: center;
      margin-bottom: 2rem;
      padding-bottom: 1rem;
      border-bottom: 1px solid var(--card-border);
    }
    .brand {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }
    .brand h1 {
      font-size: 1.5rem;
      font-weight: 700;
      letter-spacing: -0.025em;
    }
    .status-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.35rem 0.85rem;
      border-radius: 9999px;
      font-size: 0.85rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
    }
    .status-badge.connected { background: var(--success-bg); color: var(--success); border: 1px solid rgba(16, 185, 129, 0.3); }
    .status-badge.connecting { background: var(--warning-bg); color: var(--warning); border: 1px solid rgba(245, 158, 11, 0.3); }
    .status-badge.not_configured { background: rgba(148, 163, 184, 0.1); color: var(--text-muted); border: 1px solid var(--card-border); }
    .status-badge.error, .status-badge.disconnected { background: var(--danger-bg); color: var(--danger); border: 1px solid rgba(244, 63, 94, 0.3); }
    
    .status-dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      background: currentColor;
    }
    .status-badge.connected .status-dot { box-shadow: 0 0 8px var(--success); }

    .grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 1.5rem;
      margin-bottom: 1.5rem;
    }
    @media (max-width: 768px) {
      .grid { grid-template-columns: 1fr; }
    }

    .card {
      background: var(--card-bg);
      border: 1px solid var(--card-border);
      border-radius: 0.75rem;
      padding: 1.5rem;
    }
    .card h2 {
      font-size: 1.15rem;
      margin-bottom: 1rem;
      font-weight: 600;
      display: flex;
      justify-content: space-between;
      align-items: center;
    }
    
    .form-group {
      margin-bottom: 1rem;
    }
    .form-group label {
      display: block;
      font-size: 0.85rem;
      font-weight: 500;
      color: var(--text-muted);
      margin-bottom: 0.35rem;
    }
    .form-group input {
      width: 100%;
      background: #0f172a;
      border: 1px solid var(--card-border);
      border-radius: 0.375rem;
      padding: 0.6rem 0.75rem;
      color: var(--text);
      font-size: 0.9rem;
      font-family: var(--mono);
      outline: none;
      transition: border-color 0.2s;
    }
    .form-group input:focus {
      border-color: var(--accent);
    }
    .token-wrapper {
      position: relative;
    }
    .toggle-pwd {
      position: absolute;
      right: 10px;
      top: 50%;
      transform: translateY(-50%);
      background: none;
      border: none;
      color: var(--text-muted);
      cursor: pointer;
      font-size: 0.8rem;
    }
    
    .btn {
      background: var(--accent);
      color: #0f172a;
      font-weight: 600;
      padding: 0.65rem 1.25rem;
      border-radius: 0.375rem;
      border: none;
      cursor: pointer;
      font-size: 0.9rem;
      transition: background 0.2s;
    }
    .btn:hover { background: var(--accent-hover); }
    .btn-secondary {
      background: #1e293b;
      color: var(--text);
      border: 1px solid var(--card-border);
    }
    .btn-secondary:hover { background: #334155; }
    
    .tools-list {
      list-style: none;
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }
    .tool-item {
      display: flex;
      justify-content: space-between;
      align-items: center;
      padding: 0.6rem 0.75rem;
      background: #0f172a;
      border: 1px solid var(--card-border);
      border-radius: 0.375rem;
      font-size: 0.9rem;
    }
    .tool-name { font-weight: 500; }
    .tool-meta { font-family: var(--mono); font-size: 0.8rem; color: var(--text-muted); }
    .tool-badge {
      font-size: 0.75rem;
      padding: 0.2rem 0.5rem;
      border-radius: 4px;
      font-weight: 600;
    }
    .tool-badge.ok { background: var(--success-bg); color: var(--success); }
    .tool-badge.missing { background: rgba(148, 163, 184, 0.1); color: var(--text-muted); }

    .active-job-card {
      background: rgba(56, 189, 248, 0.05);
      border: 1px solid rgba(56, 189, 248, 0.3);
      border-radius: 0.75rem;
      padding: 1.25rem;
      margin-bottom: 1.5rem;
      display: none;
    }
    .active-job-card.visible { display: block; }
    
    .table-wrapper {
      overflow-x: auto;
    }
    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 0.85rem;
      margin-top: 0.5rem;
    }
    th, td {
      text-align: left;
      padding: 0.65rem 0.75rem;
      border-bottom: 1px solid var(--card-border);
    }
    th {
      color: var(--text-muted);
      font-weight: 600;
    }
    .mono { font-family: var(--mono); }
    .badge-sm {
      display: inline-block;
      padding: 0.15rem 0.4rem;
      border-radius: 4px;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
    }
    .badge-sm.success { background: var(--success-bg); color: var(--success); }
    .badge-sm.failed { background: var(--danger-bg); color: var(--danger); }
    .badge-sm.running { background: var(--warning-bg); color: var(--warning); }

    .banner-error {
      background: var(--danger-bg);
      color: var(--danger);
      border: 1px solid rgba(244, 63, 94, 0.3);
      padding: 0.75rem 1rem;
      border-radius: 0.5rem;
      margin-bottom: 1.5rem;
      display: none;
      font-size: 0.9rem;
    }
    .banner-error.visible { display: block; }
    
    .toast {
      position: fixed;
      bottom: 2rem;
      right: 2rem;
      background: var(--card-bg);
      border: 1px solid var(--accent);
      color: var(--text);
      padding: 0.75rem 1.25rem;
      border-radius: 0.5rem;
      box-shadow: 0 10px 25px rgba(0,0,0,0.5);
      display: none;
      z-index: 100;
    }
  </style>
</head>
<body>
  <div class="container">
    <header>
      <div class="brand">
        <span style="font-size: 1.75rem;">🐙</span>
        <div>
          <h1>Octopus Runner</h1>
          <p style="font-size: 0.8rem; color: var(--text-muted);">Dev Machine Execution Node</p>
        </div>
      </div>
      <div id="status-badge" class="status-badge not_configured">
        <span class="status-dot"></span>
        <span id="status-text">Loading...</span>
      </div>
    </header>

    <div id="error-banner" class="banner-error"></div>

    <div id="active-job" class="active-job-card">
      <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 0.5rem;">
        <strong style="color: var(--accent);">⚡ Active Execution</strong>
        <span class="badge-sm running">RUNNING</span>
      </div>
      <div style="display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 0.5rem; font-size: 0.85rem;">
        <div><span class="tool-meta">Job ID:</span> <span id="aj-id" class="mono"></span></div>
        <div><span class="tool-meta">Project:</span> <span id="aj-project" class="mono"></span></div>
        <div><span class="tool-meta">Type:</span> <span id="aj-type" class="mono"></span></div>
        <div><span class="tool-meta">Started:</span> <span id="aj-started"></span></div>
      </div>
    </div>

    <div class="grid">
      <!-- Config Form -->
      <div class="card">
        <h2>
          <span>⚙️ Runner Configuration</span>
          <button type="button" class="btn btn-secondary" style="padding: 0.3rem 0.6rem; font-size: 0.75rem;" onclick="triggerReconnect()">Reconnect</button>
        </h2>
        <form id="config-form" onsubmit="saveConfig(event)">
          <div class="form-group">
            <label for="server_url">Server WebSocket URL</label>
            <input type="text" id="server_url" name="server_url" placeholder="ws://192.168.1.50:8080/runner/connect" required>
          </div>
          <div class="form-group">
            <label for="runner_token">Runner Token (from Octopus Web UI > Runners)</label>
            <div class="token-wrapper">
              <input type="password" id="runner_token" name="runner_token" placeholder="Paste generated token" required>
              <button type="button" class="toggle-pwd" onclick="toggleTokenVisibility()">Show</button>
            </div>
          </div>
          <div class="form-group">
            <label for="clone_cache_dir">Clone Cache Directory</label>
            <input type="text" id="clone_cache_dir" name="clone_cache_dir" value="~/.octopus/clones">
          </div>
          <div class="form-group">
            <label for="local_queue_db">Local Queue Database</label>
            <input type="text" id="local_queue_db" name="local_queue_db" value="~/.octopus/runner.db">
          </div>
          <div class="form-group">
            <label for="web_port">Local Web UI Port</label>
            <input type="number" id="web_port" name="web_port" value="8088">
          </div>
          <button type="submit" class="btn" style="width: 100%;">Save &amp; Connect</button>
        </form>
      </div>

      <!-- Environment / Health Check -->
      <div class="card">
        <h2>🛠️ Machine Toolchain Health</h2>
        <ul id="tools-list" class="tools-list">
          <li class="tool-item"><span class="tool-name">Checking toolchain...</span></li>
        </ul>
      </div>
    </div>

    <!-- Job History -->
    <div class="card">
      <h2>📋 Execution History</h2>
      <div class="table-wrapper">
        <table>
          <thead>
            <tr>
              <th>Status</th>
              <th>Type</th>
              <th>Project</th>
              <th>Job ID</th>
              <th>Duration</th>
              <th>Time</th>
            </tr>
          </thead>
          <tbody id="history-body">
            <tr><td colspan="6" style="text-align: center; color: var(--text-muted);">No jobs executed yet</td></tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>

  <div id="toast" class="toast">Configuration saved!</div>

  <script>
    let isEditingForm = false;

    document.querySelectorAll('#config-form input').forEach(input => {
      input.addEventListener('focus', () => { isEditingForm = true; });
    });

    function toggleTokenVisibility() {
      const input = document.getElementById('runner_token');
      const btn = document.querySelector('.toggle-pwd');
      if (input.type === 'password') {
        input.type = 'text';
        btn.textContent = 'Hide';
      } else {
        input.type = 'password';
        btn.textContent = 'Show';
      }
    }

    function showToast(msg) {
      const t = document.getElementById('toast');
      t.textContent = msg;
      t.style.display = 'block';
      setTimeout(() => { t.style.display = 'none'; }, 3000);
    }

    async function fetchStatus() {
      try {
        const res = await fetch('/api/status');
        if (!res.ok) return;
        const data = await res.json();

        // Update status badge
        const badge = document.getElementById('status-badge');
        const statusText = document.getElementById('status-text');
        badge.className = 'status-badge ' + data.status;
        statusText.textContent = data.status.replace('_', ' ');

        // Update error banner
        const errBanner = document.getElementById('error-banner');
        if (data.last_error && data.status !== 'connected') {
          errBanner.textContent = '⚠️ ' + data.last_error;
          errBanner.classList.add('visible');
        } else {
          errBanner.classList.remove('visible');
        }

        // Fill form if not currently active
        if (!isEditingForm) {
          if (data.config.server_url) document.getElementById('server_url').value = data.config.server_url;
          if (data.config.runner_token) document.getElementById('runner_token').value = data.config.runner_token;
          if (data.config.clone_cache_dir) document.getElementById('clone_cache_dir').value = data.config.clone_cache_dir;
          if (data.config.local_queue_db) document.getElementById('local_queue_db').value = data.config.local_queue_db;
          if (data.config.web_port) document.getElementById('web_port').value = data.config.web_port;
        }

        // Active Job
        const ajCard = document.getElementById('active-job');
        if (data.active_job) {
          ajCard.classList.add('visible');
          document.getElementById('aj-id').textContent = data.active_job.id;
          document.getElementById('aj-project').textContent = data.active_job.project_id;
          document.getElementById('aj-type').textContent = data.active_job.type;
          document.getElementById('aj-started').textContent = new Date(data.active_job.started_at).toLocaleTimeString();
        } else {
          ajCard.classList.remove('visible');
        }

        // Tools
        const toolsList = document.getElementById('tools-list');
        if (data.tools && data.tools.length > 0) {
          toolsList.innerHTML = data.tools.map(function(t) {
            var meta = t.installed ? (t.version || t.path) : 'Not found in PATH';
            var badgeClass = t.installed ? 'ok' : 'missing';
            var badgeText = t.installed ? 'INSTALLED' : 'MISSING';
            return '<li class="tool-item">' +
              '<div><div class="tool-name">' + t.name + '</div>' +
              '<div class="tool-meta">' + meta + '</div></div>' +
              '<span class="tool-badge ' + badgeClass + '">' + badgeText + '</span></li>';
          }).join('');
        }

        // History
        const histBody = document.getElementById('history-body');
        if (data.history && data.history.length > 0) {
          histBody.innerHTML = data.history.map(function(h) {
            var shortId = h.id ? (h.id.length > 8 ? h.id.substring(0, 8) + '...' : h.id) : '-';
            var timeStr = h.started_at ? new Date(h.started_at).toLocaleTimeString() : '-';
            return '<tr>' +
              '<td><span class="badge-sm ' + h.status + '">' + h.status + '</span></td>' +
              '<td class="mono">' + (h.type || '-') + '</td>' +
              '<td class="mono">' + (h.project_id || '-') + '</td>' +
              '<td class="mono" title="' + (h.id || '') + '">' + shortId + '</td>' +
              '<td>' + (h.duration || '-') + '</td>' +
              '<td>' + timeStr + '</td>' +
              '</tr>';
          }).join('');
        } else {
          histBody.innerHTML = '<tr><td colspan="6" style="text-align: center; color: var(--text-muted);">No jobs executed yet</td></tr>';
        }
      } catch (e) {
        console.error("status fetch error", e);
      }
    }

    async function saveConfig(e) {
      e.preventDefault();
      const payload = {
        server_url: document.getElementById('server_url').value,
        runner_token: document.getElementById('runner_token').value,
        clone_cache_dir: document.getElementById('clone_cache_dir').value,
        local_queue_db: document.getElementById('local_queue_db').value,
        web_port: parseInt(document.getElementById('web_port').value, 10) || 8088
      };

      try {
        const res = await fetch('/api/config', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload)
        });
        if (res.ok) {
          isEditingForm = false;
          showToast('Configuration saved! Reconnecting...');
          fetchStatus();
        } else {
          const err = await res.text();
          alert('Error saving config: ' + err);
        }
      } catch (err) {
        alert('Failed to save config: ' + err.message);
      }
    }

    async function triggerReconnect() {
      await fetch('/api/reconnect', { method: 'POST' });
      showToast('Reconnection requested...');
      fetchStatus();
    }

    fetchStatus();
    setInterval(fetchStatus, 2500);
  </script>
</body>
</html>
`
