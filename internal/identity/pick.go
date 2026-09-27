package identity

import (
	"slices"
	"strings"
	"time"
)

// Candidate is a GitHub user that one email address maps to.
type Candidate struct {
	UserID int64
	Login  string
	Seated bool
	// LastActivityAt is the latest seat activity across scopes; nil when the
	// user has no seat or never used it.
	LastActivityAt *time.Time
}

// Pick chooses one login for an address shared by several GitHub accounts:
// a seated user first, then the most recent seat activity, then the login in
// case-insensitive order. It returns the chosen candidate and the logins not
// chosen, in the same order. ok is false when cands is empty.
func Pick(cands []Candidate) (chosen Candidate, others []string, ok bool) {
	if len(cands) == 0 {
		return Candidate{}, nil, false
	}
	sorted := slices.Clone(cands)
	slices.SortStableFunc(sorted, compare)
	for _, c := range sorted[1:] {
		others = append(others, c.Login)
	}
	return sorted[0], others, true
}

func compare(a, b Candidate) int {
	if a.Seated != b.Seated {
		if a.Seated {
			return -1
		}
		return 1
	}
	switch {
	case a.LastActivityAt != nil && b.LastActivityAt == nil:
		return -1
	case a.LastActivityAt == nil && b.LastActivityAt != nil:
		return 1
	case a.LastActivityAt != nil && !a.LastActivityAt.Equal(*b.LastActivityAt):
		return b.LastActivityAt.Compare(*a.LastActivityAt)
	}
	if c := strings.Compare(strings.ToLower(a.Login), strings.ToLower(b.Login)); c != 0 {
		return c
	}
	return strings.Compare(a.Login, b.Login)
}
