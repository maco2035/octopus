package web

import (
	"errors"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"octopus/internal/domain"
	"octopus/internal/store"
)

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	// Already logged in? Skip straight past the form.
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if sess, err := s.Store.LoadSession(r.Context(), cookie.Value); err == nil && time.Now().Before(sess.ExpiresAt) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	s.render(w, "login.html", map[string]any{"Title": "Log in"})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")

	user, err := s.Store.LoadUserByUsername(r.Context(), username)
	if errors.Is(err, store.ErrNotFound) {
		s.render(w, "login.html", map[string]any{"Title": "Log in", "Error": "Invalid username or password"})
		return
	}
	if err != nil {
		serverError(w, r, err)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		s.render(w, "login.html", map[string]any{"Title": "Log in", "Error": "Invalid username or password"})
		return
	}

	token := newSessionToken()
	expiresAt := time.Now().Add(sessionTTL)
	if err := s.Store.SaveSession(r.Context(), &domain.Session{Token: token, UserID: user.ID, ExpiresAt: expiresAt}); err != nil {
		serverError(w, r, err)
		return
	}

	s.setSessionCookie(w, token, expiresAt)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		_ = s.Store.DeleteSession(r.Context(), cookie.Value)
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
