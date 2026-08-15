package web

import (
	"encoding/json"
	"html/template"
	"net/http"

	"github.com/google/uuid"

	"octopus/internal/domain"
	"octopus/internal/engine"
)

// marshalDef returns template.JS rather than a plain string so pipeline_editor.html
// can embed it verbatim inside <script id="pipeline-data">. A plain string
// there gets run through html/template's JS-string escaper like any other
// script-context value — regardless of the tag's type="application/json" —
// which double-encodes it into a JSON string *containing* JSON text; on
// load, JSON.parse would then hand canvas.js a string instead of an
// object, silently dropping every node and edge. json.Marshal already
// HTML-escapes '<', '>', and '&', so emitting it unescaped here is safe.
func marshalDef(def *domain.PipelineDef) (template.JS, error) {
	b, err := json.Marshal(def)
	if err != nil {
		return "", err
	}
	return template.JS(b), nil
}

func (s *Server) handleAgentTypes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Registry.ListTypes())
}

type saveDefRequest struct {
	ID        string           `json:"id"`
	ProjectID string           `json:"project_id"`
	Name      string           `json:"name"`
	Nodes     []domain.NodeDef `json:"nodes"`
	Edges     []domain.EdgeDef `json:"edges"`
}

func (s *Server) handleSavePipelineDef(w http.ResponseWriter, r *http.Request) {
	var req saveDefRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ProjectID == "" || req.Name == "" {
		http.Error(w, "project_id and name are required", http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		req.ID = uuid.NewString()
	}

	def := &domain.PipelineDef{ID: req.ID, ProjectID: req.ProjectID, Name: req.Name, Nodes: req.Nodes, Edges: req.Edges}
	if _, err := engine.Levels(def); err != nil {
		http.Error(w, "invalid pipeline: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.Store.SavePipelineDef(r.Context(), def); err != nil {
		serverError(w, r, err, "project_id", req.ProjectID)
		return
	}

	// The first pipeline a project gets becomes its default automatically
	// — what the Slack slash command runs when only given a project +
	// ticket (PLAN.md Phase 5). Later pipelines don't reassign it; use
	// "make default" on the project page for that.
	if project, err := s.Store.LoadProject(r.Context(), req.ProjectID); err == nil && project.DefaultPipelineDefID == "" {
		project.DefaultPipelineDefID = def.ID
		_ = s.Store.SaveProject(r.Context(), project)
	}

	writeJSON(w, http.StatusOK, map[string]string{"id": def.ID})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
