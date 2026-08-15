package ratelimit_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"octopus/internal/ratelimit"
)

func TestLimiter_AllowsUpToLimitThenBlocks(t *testing.T) {
	l := ratelimit.New(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.Allow("key1") {
			t.Fatalf("expected attempt %d to be allowed", i+1)
		}
	}
	if l.Allow("key1") {
		t.Fatal("expected the 4th attempt within the window to be blocked")
	}
}

func TestLimiter_KeysAreIndependent(t *testing.T) {
	l := ratelimit.New(1, time.Minute)
	if !l.Allow("a") {
		t.Fatal("expected first call for key a to be allowed")
	}
	if !l.Allow("b") {
		t.Fatal("expected key b to have its own independent budget")
	}
	if l.Allow("a") {
		t.Fatal("expected second call for key a to be blocked")
	}
}

func TestLimiter_WindowExpires(t *testing.T) {
	l := ratelimit.New(1, 30*time.Millisecond)
	if !l.Allow("k") {
		t.Fatal("expected first call to be allowed")
	}
	if l.Allow("k") {
		t.Fatal("expected immediate second call to be blocked")
	}
	time.Sleep(40 * time.Millisecond)
	if !l.Allow("k") {
		t.Fatal("expected call after the window expired to be allowed again")
	}
}

func TestLimiter_Middleware(t *testing.T) {
	l := ratelimit.New(1, time.Minute)
	handler := l.Middleware(ratelimit.RemoteAddrKey, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "1.2.3.4:5555"

	rec1 := httptest.NewRecorder()
	handler(rec1, req)
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected first request to succeed, got %d", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	handler(rec2, req)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected second request to be rate limited, got %d", rec2.Code)
	}

	// A different client (different port stripped, different IP) gets its own budget.
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.RemoteAddr = "9.9.9.9:1111"
	rec3 := httptest.NewRecorder()
	handler(rec3, req2)
	if rec3.Code != http.StatusOK {
		t.Fatalf("expected a different client to have its own budget, got %d", rec3.Code)
	}
}
