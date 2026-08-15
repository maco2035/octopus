package domain_test

import (
	"strings"
	"testing"

	"octopus/internal/domain"
)

func TestValidSlug(t *testing.T) {
	valid := []string{"proj1", "my-project", "a", "PROJECT_1", strings.Repeat("a", 64)}
	for _, s := range valid {
		if !domain.ValidSlug(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}

	invalid := []string{
		"", "-leading-dash", "_leading-underscore",
		"../../etc/passwd", "../secret", "a/b", "a b", "a.b",
		strings.Repeat("a", 65), // over the length cap
		"proj\x00null",
	}
	for _, s := range invalid {
		if domain.ValidSlug(s) {
			t.Errorf("expected %q to be rejected", s)
		}
	}
}

func TestValidTicketID(t *testing.T) {
	valid := []string{"TICKET-123", "abc.def_1", "1"}
	for _, s := range valid {
		if !domain.ValidTicketID(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}

	// Every one of these, if it reached tools.GitRunner unvalidated, would
	// be passed as a bare positional argument to git (branch name in
	// checkout/push/merge) — a leading "-" risks being parsed as a flag
	// instead of a ref name.
	invalid := []string{
		"", "--force", "-o", "-x", "--upload-pack=/bin/sh",
		"../../etc/passwd", "a b", "a/b",
	}
	for _, s := range invalid {
		if domain.ValidTicketID(s) {
			t.Errorf("expected %q to be rejected", s)
		}
	}
}
