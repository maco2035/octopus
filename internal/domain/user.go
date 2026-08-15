package domain

import "time"

// User is the v1 admin account, seeded from the server's own static config
// (octopus.yaml when run from source, environment variables when run via
// Docker/the HA add-on — see docker-entrypoint.sh) rather than a signup
// flow (PLAN.md Key Design Decision 23).
type User struct {
	ID           string
	Username     string
	PasswordHash string // bcrypt
}

// Session backs the web UI's login. A server-side row (not just a signed
// cookie) so logout/revocation is real, not "wait for expiry."
type Session struct {
	Token     string // the cookie value; random, not derived from anything guessable
	UserID    string
	ExpiresAt time.Time
}
