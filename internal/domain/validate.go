package domain

import "regexp"

// slugPattern is deliberately conservative: a Project.ID isn't just a
// database key, it's also used as a path segment when building a runner's
// local clone directory (tools.GitRunner.WorkDir joins it with
// filepath.Join). An ID containing "../" or an absolute path would let it
// escape clone_cache_dir entirely; requiring the first character to be
// alphanumeric and restricting the rest to a small safe charset closes
// that off rather than trying to blocklist "../" and hoping nothing else
// is dangerous.
var slugPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// ValidSlug reports whether s is safe to use as a Project or PipelineDef
// ID — both end up as path segments (URLs, and for Project.ID, local
// filesystem paths under clone_cache_dir).
func ValidSlug(s string) bool {
	return slugPattern.MatchString(s)
}

// ticketIDPattern is similarly conservative: TicketID is substituted
// directly into a git branch name (BranchPattern's {ticket_id}) and that
// branch name is then passed as a bare positional argument to git
// checkout/push/merge. A value starting with "-" could be parsed by git
// as an option rather than a ref name (e.g. a ticket_id of "--force"
// reaching a push); requiring the first character to be alphanumeric
// rules that out. This also happens to be the least-trusted input
// boundary in the whole system — the Slack slash command accepts it from
// anyone in the workspace who can run /octopus, not just the logged-in
// admin.
var ticketIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// ValidTicketID reports whether s is safe to substitute into a branch
// name and pass to git.
func ValidTicketID(s string) bool {
	return ticketIDPattern.MatchString(s)
}
