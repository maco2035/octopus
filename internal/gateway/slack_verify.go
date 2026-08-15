package gateway

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"time"
)

// replayWindow rejects a request whose timestamp is further than this from
// now in either direction — Slack's own recommended value, and what stops
// a captured request from being replayed indefinitely.
const replayWindow = 5 * time.Minute

// verifySignature checks Slack's HMAC-SHA256 request signature (the
// v0=... in X-Slack-Signature, over "v0:{timestamp}:{raw body}") and the
// timestamp replay window before calling next. Slack signs the exact raw
// body, so this reads it once, verifies, then restores it for the real
// handler (r.ParseForm et al.) to read again.
func (g *Gateway) verifySignature(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		tsHeader := r.Header.Get("X-Slack-Request-Timestamp")
		sigHeader := r.Header.Get("X-Slack-Signature")
		if tsHeader == "" || sigHeader == "" {
			http.Error(w, "missing signature headers", http.StatusUnauthorized)
			return
		}

		ts, err := strconv.ParseInt(tsHeader, 10, 64)
		if err != nil {
			http.Error(w, "bad timestamp", http.StatusUnauthorized)
			return
		}
		age := time.Since(time.Unix(ts, 0))
		if age > replayWindow || age < -replayWindow {
			http.Error(w, "stale request", http.StatusUnauthorized)
			return
		}

		mac := hmac.New(sha256.New, []byte(g.SigningSecret))
		mac.Write([]byte("v0:" + tsHeader + ":"))
		mac.Write(body)
		expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

		if !hmac.Equal([]byte(expected), []byte(sigHeader)) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}
