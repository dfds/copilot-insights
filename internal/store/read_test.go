package store_test

import (
	"slices"
	"testing"
	"time"

	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/identity"
	"go.dfds.cloud/copilot-insights/internal/store"
)

func TestScopesAndStatuses(t *testing.T) {
	s, ctx := migrated(t)
	if err := s.SyncScopes(ctx, []store.ScopeState{
		{Scope: gh.Org("b"), InstallationID: 1}, {Scope: gh.Org("a"), InstallationID: 2}, {Scope: gh.Enterprise("dfds")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncScopes(ctx, []store.ScopeState{{Scope: gh.Org("a"), InstallationID: 2}, {Scope: gh.Enterprise("dfds")}}); err != nil {
		t.Fatal(err)
	}
	scopes, err := s.Scopes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, sc := range scopes {
		got = append(got, sc.Scope().String()+map[bool]string{true: " installed", false: ""}[sc.Installed]+
			map[bool]string{true: "", false: " inactive"}[sc.Active])
	}
	if want := []string{"enterprise:dfds", "org:a installed", "org:b installed inactive"}; !slices.Equal(got, want) {
		t.Errorf("scopes = %v", got)
	}
	if loc := scopes[0].FirstSeenAt.Location(); loc != time.UTC {
		t.Errorf("timestamptz scanned in %v, want UTC", loc)
	}

	for _, r := range []struct {
		collector string
		status    store.RunStatus
		items     int
	}{{"seats", store.RunOK, 5}, {"seats", store.RunError, 0}, {"members", store.RunPartial, 3}} {
		id, err := s.StartRun(ctx, r.collector)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishRun(ctx, id, r.status, r.items, map[bool]string{true: "boom"}[r.status == store.RunError]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.StartRun(ctx, "reports"); err != nil {
		t.Fatal(err)
	}
	st, err := s.CollectorStatuses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	for _, c := range st {
		line := c.Collector + " " + c.LastRun.Status
		if c.LastRun.Error != nil {
			line += " " + *c.LastRun.Error
		}
		if c.LastOkAt != nil {
			line += " ok-at"
		}
		if c.LastRun.FinishedAt == nil {
			line += " unfinished"
		}
		got = append(got, line)
	}
	if want := []string{"members partial ok-at", "reports running unfinished", "seats error boom ok-at"}; !slices.Equal(got, want) {
		t.Errorf("statuses = %v", got)
	}
}

// seedPeople: alice (1) has seats in the enterprise and dfds-internal and two
// accounts share bob's address; carol (3) has an org seat only; dave (4) has
// no seat.
func seedPeople(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.ReplaceOrgMembers(ctx, map[string][]store.OrgMember{
		"dfds-internal": {
			member(1, "alice", verified("alice@dfds.com")),
			member(2, "bob", verified("bob@dfds.com")),
			member(5, "bob_2", verified("bob@dfds.com")),
		},
		"dfds": {member(3, "carol", profile("carol@dfds.com")), member(4, "dave")},
	}); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	ent := []gh.Seat{
		{UserID: 1, Login: "alice", OrgName: "dfds-internal", LastActivityAt: ts("2026-09-20T10:00:00Z")},
		{UserID: 2, Login: "bob", OrgName: "dfds-internal", LastActivityAt: ts("2026-06-01T10:00:00Z")},
		{UserID: 5, Login: "bob_2", OrgName: "dfds-internal", LastActivityAt: ts("2026-09-01T10:00:00Z"),
			PendingCancellationDate: "2026-10-01"},
	}
	if err := s.SaveSeats(ctx, gh.Enterprise("dfds"), day, ent); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSeats(ctx, gh.Enterprise("dfds"), day.AddDate(0, 0, 1), ent[:2]); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSeats(ctx, gh.Org("dfds-internal"), day, ent[:1]); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSeats(ctx, gh.Org("dfds"), day, []gh.Seat{{UserID: 3, Login: "carol", OrgName: "dfds"}}); err != nil {
		t.Fatal(err)
	}
}

func seatLogins(seats []store.Seat) []string {
	var out []string
	for _, s := range seats {
		out = append(out, s.Scope+"/"+s.Login)
	}
	return out
}

func TestSeatsQueries(t *testing.T) {
	s, ctx := migrated(t)
	seedPeople(t, s)
	ent := gh.Enterprise("dfds")

	seats, total, err := s.Seats(ctx, store.SeatFilter{Scope: ent, Page: store.Page{Limit: 1, Offset: 1}})
	if err != nil || total != 2 || !slices.Equal(seatLogins(seats), []string{"enterprise:dfds/bob"}) {
		t.Fatalf("paged = %v total=%d err=%v", seatLogins(seats), total, err)
	}
	if seats[0].Email == nil || *seats[0].Email != "bob@dfds.com" || seats[0].LastActivityAt == nil {
		t.Errorf("joined seat = %+v", seats[0])
	}
	seats, _, _ = s.Seats(ctx, store.SeatFilter{Scope: ent, Login: "ALICE", Page: store.Page{Limit: 10}})
	if !slices.Equal(seatLogins(seats), []string{"enterprise:dfds/alice"}) {
		t.Errorf("login filter = %v", seatLogins(seats))
	}
	seats, _, _ = s.Seats(ctx, store.SeatFilter{Scope: ent, InactiveSince: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Page: store.Page{Limit: 10}})
	if !slices.Equal(seatLogins(seats), []string{"enterprise:dfds/bob"}) {
		t.Errorf("inactive filter = %v", seatLogins(seats))
	}
	seats, _, _ = s.Seats(ctx, store.SeatFilter{Scope: gh.Org("dfds"), InactiveSince: time.Now(), Page: store.Page{Limit: 10}})
	if !slices.Equal(seatLogins(seats), []string{"org:dfds/carol"}) {
		t.Errorf("a never-used seat counts as inactive: %v", seatLogins(seats))
	}

	seats, err = s.UserSeats(ctx, 1)
	if err != nil || !slices.Equal(seatLogins(seats), []string{"enterprise:dfds/alice", "org:dfds-internal/alice"}) {
		t.Errorf("user seats = %v %v", seatLogins(seats), err)
	}
	if ok, err := s.HasEnterpriseSeats(ctx, "dfds"); !ok || err != nil {
		t.Errorf("HasEnterpriseSeats(dfds) = %v %v", ok, err)
	}
	if ok, _ := s.HasEnterpriseSeats(ctx, "other"); ok {
		t.Error("HasEnterpriseSeats(other) = true")
	}

	days, err := s.SeatHistory(ctx, ent, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := []store.SeatDay{
		{Day: "2026-09-22", Total: 3, Active28d: 2, PendingCancellation: 1},
		{Day: "2026-09-23", Total: 2, Active28d: 1},
	}
	if !slices.Equal(days, want) {
		t.Errorf("history = %+v", days)
	}
	if days, _ := s.SeatHistory(ctx, ent, time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)); len(days) != 1 {
		t.Errorf("single-day range = %+v", days)
	}
}

func TestIdentityQueries(t *testing.T) {
	s, ctx := migrated(t)
	seedPeople(t, s)
	all := store.Page{Limit: 100}
	logins := func(f store.IdentityFilter) []string {
		t.Helper()
		ids, total, err := s.Identities(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, id := range ids {
			out = append(out, id.Login)
		}
		if total < len(out) {
			t.Errorf("total %d < rows %d", total, len(out))
		}
		return out
	}
	if got := logins(store.IdentityFilter{Page: all}); !slices.Equal(got, []string{"alice", "bob", "bob_2", "carol", "dave"}) {
		t.Errorf("all = %v", got)
	}
	if got := logins(store.IdentityFilter{Email: "bob@dfds.com", Page: all}); !slices.Equal(got, []string{"bob", "bob_2"}) {
		t.Errorf("email = %v", got)
	}
	if got := logins(store.IdentityFilter{Q: "CAROL@", Page: all}); !slices.Equal(got, []string{"carol"}) {
		t.Errorf("q on email = %v", got)
	}
	if got := logins(store.IdentityFilter{Q: "b_", Page: all}); !slices.Equal(got, []string{"bob_2"}) {
		t.Errorf("q escapes _ : %v", got)
	}
	if got := logins(store.IdentityFilter{Q: "N dav", Page: all}); !slices.Equal(got, []string{"dave"}) {
		t.Errorf("q on name = %v", got)
	}
	// Every email contains "c" (.com), but only carol starts a login, name word or email with it.
	if got := logins(store.IdentityFilter{Q: "c", Page: all}); !slices.Equal(got, []string{"carol", "alice", "bob", "bob_2"}) {
		t.Errorf("q ranks prefix matches first = %v", got)
	}
	if got := logins(store.IdentityFilter{Q: "BOB", Page: store.Page{Limit: 1}}); !slices.Equal(got, []string{"bob"}) {
		t.Errorf("q ranks exact login first = %v", got)
	}

	id, err := s.Identity(ctx, 0, "Alice")
	if err != nil || id == nil {
		t.Fatalf("Identity = %v %v", id, err)
	}
	if !id.Seated || !slices.Equal(id.Orgs, []string{"dfds-internal"}) || len(id.Emails) != 1 ||
		id.Emails[0] != (store.IdentityEmail{Email: "alice@dfds.com", Source: "verified", Org: "dfds-internal"}) {
		t.Errorf("alice = %+v", id)
	}
	if id, _ := s.Identity(ctx, 4, ""); id == nil || id.Seated || len(id.Emails) != 0 || id.PrimaryEmail != nil {
		t.Errorf("dave = %+v", id)
	}
	if id, err := s.Identity(ctx, 0, "nobody"); id != nil || err != nil {
		t.Errorf("missing = %v %v", id, err)
	}

	cands, err := s.EmailCandidates(ctx, "bob@dfds.com")
	if err != nil || len(cands) != 2 {
		t.Fatalf("candidates = %+v %v", cands, err)
	}
	slices.SortFunc(cands, func(a, b identity.Candidate) int { return int(a.UserID - b.UserID) })
	if !cands[0].Seated || !cands[0].LastActivityAt.Equal(*ts("2026-06-01T10:00:00Z")) || cands[1].Login != "bob_2" {
		t.Errorf("candidates = %+v", cands)
	}
	if cands, _ := s.EmailCandidates(ctx, "carol@dfds.com"); len(cands) != 1 || !cands[0].Seated || cands[0].LastActivityAt != nil {
		t.Errorf("carol = %+v", cands)
	}
	if cands, _ := s.EmailCandidates(ctx, "nobody@dfds.com"); len(cands) != 0 {
		t.Errorf("nobody = %+v", cands)
	}
}
