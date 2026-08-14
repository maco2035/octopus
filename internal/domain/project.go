package domain

// Project scopes every pipeline definition and run — it's what "multiple
// projects at once" (PLAN.md Key Design Decision 12) hangs off of.
type Project struct {
	ID           string
	Name         string
	GitRemoteURL string
	BaseBranch   string
	Owner        string // nullable/empty for now; forward-compat for multi-person later
}
