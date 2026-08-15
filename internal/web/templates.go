package web

import (
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"

	"octopus/internal/domain"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

var templates = template.Must(template.ParseFS(templateFS, "templates/*.html"))

func staticSubFS() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // static/ is embedded at compile time; a missing dir is a build-time bug, not a runtime one
	}
	return sub
}

// render executes the named template against base.html, injecting the
// current user (nil on the login page) so nav.html can decide whether to
// show a logout link.
func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	if _, ok := data["User"]; !ok {
		data["User"] = (*domain.User)(nil)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		// Same reasoning as serverError: the client gets a generic
		// message, the real error (which could reference internal
		// template/field names) goes to the log only.
		slog.Error("web: template execution failed", "template", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func (s *Server) renderWithUser(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["User"] = userFromContext(r.Context())
	s.render(w, name, data)
}
