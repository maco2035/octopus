package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"octopus/internal/domain"
)

const sessionCookieName = "octopus_session"
const sessionTTL = 7 * 24 * time.Hour

type ctxKey int

const userCtxKey ctxKey = 0

// newSessionToken generates a random, unguessable session token — the
// cookie value is the token itself, but the server-side Session row (not
// the cookie's mere presence) is what makes logout/revocation real
// (PLAN.md Key Design Decision 22).
func newSessionToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// requireLogin wraps a handler so every request must carry a valid,
// unexpired session; otherwise it redirects to /login. Exempt routes
// (health check, Slack webhooks, the future runner-connect endpoint) are
// simply never wrapped with this by routes.go, rather than special-cased
// here.
func (s *Server) requireLogin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		sess, err := s.Store.LoadSession(r.Context(), cookie.Value)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if time.Now().After(sess.ExpiresAt) {
			_ = s.Store.DeleteSession(r.Context(), cookie.Value)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		user, err := s.Store.LoadUserByID(r.Context(), sess.UserID)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		ctx := context.WithValue(r.Context(), userCtxKey, user)
		next(w, r.WithContext(ctx))
	}
}

func userFromContext(ctx context.Context) *domain.User {
	u, _ := ctx.Value(userCtxKey).(*domain.User)
	return u
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time) {
	secure := (r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https") && !s.InsecureCookies
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	secure := (r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https") && !s.InsecureCookies
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}
