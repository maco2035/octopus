package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"octopus/internal/domain"
	"octopus/internal/store"
)

// serverError logs a 500-class failure with structured fields — run_id and
// project_id when the caller has them, so a failure is traceable back to
// the specific run without grepping raw error text (PLAN.md Phase 8:
// "structured logging with run ID and project ID on every line") — and
// writes a generic response. kv is extra slog key/value pairs, e.g.
// "run_id", runID.
//
// The response body is deliberately NOT err.Error() — a wrapped internal
// error can easily contain a filesystem path (clone_cache_dir layout), a
// SQL fragment, or other implementation detail that's useful to an
// attacker probing the deployment and useless to a legitimate user, who
// can't act on it anyway. The real error goes to the server log, which is
// exactly where whoever's debugging it should be looking.
func serverError(w http.ResponseWriter, r *http.Request, err error, kv ...any) {
	args := append([]any{"method", r.Method, "path", r.URL.Path, "error", err}, kv...)
	slog.Error("web: request failed", args...)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func (s *Server) handleProjectsList(w http.ResponseWriter, r *http.Request) {
	projects, err := s.Store.ListProjects(r.Context())
	if err != nil {
		serverError(w, r, err)
		return
	}
	s.renderWithUser(w, r, "projects.html", map[string]any{"Projects": projects})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	p := &domain.Project{
		ID:           r.FormValue("id"),
		Name:         r.FormValue("name"),
		GitRemoteURL: r.FormValue("git_remote_url"),
		BaseBranch:   r.FormValue("base_branch"),
	}
	if p.ID == "" || p.Name == "" {
		http.Error(w, "id and name are required", http.StatusBadRequest)
		return
	}
	// The HTML form's pattern="[a-z0-9-]+" is a UX hint, not a security
	// boundary — anyone can POST here directly. p.ID becomes a filesystem
	// path segment under clone_cache_dir (tools.GitRunner.WorkDir), so an
	// unvalidated value here is a directory-traversal vector, not just a
	// cosmetic one.
	if !domain.ValidSlug(p.ID) {
		http.Error(w, "id must match ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$", http.StatusBadRequest)
		return
	}
	if err := s.Store.SaveProject(r.Context(), p); err != nil {
		serverError(w, r, err, "project_id", p.ID)
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

func (s *Server) handleProjectDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project, err := s.Store.LoadProject(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, r, err, "project_id", id)
		return
	}

	defs, err := s.Store.ListPipelineDefs(r.Context(), id)
	if err != nil {
		serverError(w, r, err, "project_id", id)
		return
	}

	active, err := s.Store.ListActiveRuns(r.Context())
	if err != nil {
		serverError(w, r, err, "project_id", id)
		return
	}
	var runs []*domain.PipelineState
	for _, run := range active {
		if run.ProjectID == id {
			runs = append(runs, run)
		}
	}

	s.renderWithUser(w, r, "project_detail.html", map[string]any{
		"Project": project, "PipelineDefs": defs, "Runs": runs,
	})
}

// handlePipelineEditor serves the drag-and-drop canvas for either a new
// pipeline (no defID in the path) or an existing one.
func (s *Server) handlePipelineEditor(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	project, err := s.Store.LoadProject(r.Context(), projectID)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, r, err, "project_id", projectID)
		return
	}

	def := &domain.PipelineDef{ProjectID: projectID}
	if defID := r.PathValue("defID"); defID != "" {
		def, err = s.Store.LoadPipelineDef(r.Context(), defID)
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			serverError(w, r, err, "project_id", projectID)
			return
		}
	}

	defJSON, err := marshalDef(def)
	if err != nil {
		serverError(w, r, err, "project_id", projectID)
		return
	}

	s.renderWithUser(w, r, "pipeline_editor.html", map[string]any{
		"Project": project, "Def": def, "DefJSON": defJSON, "AgentTypes": s.Registry.ListTypes(),
	})
}

func (s *Server) handleTriggerRun(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	defID := r.PathValue("defID")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ticketID := r.FormValue("ticket_id")
	// TicketID gets substituted into a branch name and passed to git as a
	// bare positional argument (checkout/push/merge) — an unvalidated
	// value starting with "-" could be parsed by git as a flag instead of
	// a ref name. See domain.ValidTicketID's doc comment.
	if !domain.ValidTicketID(ticketID) {
		http.Error(w, "ticket_id must match ^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$", http.StatusBadRequest)
		return
	}

	state, err := s.Scheduler.StartRun(r.Context(), projectID, defID, ticketID)
	if err != nil {
		serverError(w, r, err, "project_id", projectID, "pipeline_def_id", defID, "ticket_id", ticketID)
		return
	}
	http.Redirect(w, r, "/runs/"+state.RunID, http.StatusSeeOther)
}

// handleMakeDefaultPipeline sets which PipelineDef the Slack slash command
// runs for this project when only given a project + ticket. The first
// saved pipeline already becomes the default automatically
// (handleSavePipelineDef); this is how to change it later.
func (s *Server) handleMakeDefaultPipeline(w http.ResponseWriter, r *http.Request) {
	projectID, defID := r.PathValue("id"), r.PathValue("defID")
	project, err := s.Store.LoadProject(r.Context(), projectID)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, r, err, "project_id", projectID)
		return
	}
	project.DefaultPipelineDefID = defID
	if err := s.Store.SaveProject(r.Context(), project); err != nil {
		serverError(w, r, err, "project_id", projectID)
		return
	}
	http.Redirect(w, r, "/projects/"+projectID, http.StatusSeeOther)
}

func (s *Server) handleRunStatus(w http.ResponseWriter, r *http.Request) {
	run, err := s.loadRun(w, r)
	if err != nil {
		return
	}
	s.renderWithUser(w, r, "run_status.html", map[string]any{"Run": run, "PendingOutput": pendingOutput(run)})
}

func (s *Server) handleRunFragment(w http.ResponseWriter, r *http.Request) {
	run, err := s.loadRun(w, r)
	if err != nil {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "run_fragment", map[string]any{"Run": run, "PendingOutput": pendingOutput(run)}); err != nil {
		serverError(w, r, err, "run_id", run.RunID, "project_id", run.ProjectID)
	}
}

func (s *Server) handleRunReview(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	actionToken := r.FormValue("action_token")
	decision := r.FormValue("decision")

	run, err := s.Store.Load(r.Context(), runID)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, r, err, "run_id", runID)
		return
	}

	var edited map[string]any
	approve := decision == "approve" || decision == "edit"
	if decision == "edit" && run.PendingNodeID != "" {
		edited = map[string]any{run.PendingNodeID: r.FormValue("edited_output")}
	}

	if err := s.Scheduler.Continue(r.Context(), runID, actionToken, approve, edited); err != nil {
		if errors.Is(err, store.ErrStaleActionToken) {
			http.Redirect(w, r, "/runs/"+runID, http.StatusSeeOther)
			return
		}
		serverError(w, r, err, "run_id", runID, "project_id", run.ProjectID)
		return
	}
	http.Redirect(w, r, "/runs/"+runID, http.StatusSeeOther)
}

func (s *Server) handleRunContinue(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	prior, err := s.Store.Load(r.Context(), runID)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, r, err, "run_id", runID)
		return
	}

	next, err := s.Scheduler.StartContinuation(r.Context(), prior.ProjectID, prior.PipelineDefID, fmt.Sprintf("%s-continued", prior.TicketID), runID)
	if err != nil {
		serverError(w, r, err, "run_id", runID, "project_id", prior.ProjectID)
		return
	}
	http.Redirect(w, r, "/runs/"+next.RunID, http.StatusSeeOther)
}

func (s *Server) loadRun(w http.ResponseWriter, r *http.Request) (*domain.PipelineState, error) {
	runID := r.PathValue("runID")
	run, err := s.Store.Load(r.Context(), runID)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, err
	}
	if err != nil {
		serverError(w, r, err, "run_id", runID)
		return nil, err
	}
	return run, nil
}

func pendingOutput(run *domain.PipelineState) string {
	if run.PendingNodeID == "" {
		return ""
	}
	v, _ := run.GetOutput(run.PendingNodeID)
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
