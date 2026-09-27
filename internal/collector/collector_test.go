package collector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/store"
)

func installs(scopes ...gh.Scope) []gh.Installation {
	var out []gh.Installation
	for i, s := range scopes {
		out = append(out, gh.Installation{ID: int64(100 + i), Scope: s})
	}
	return out
}

func TestMembersSyncsScopes(t *testing.T) {
	g := &fakeGitHub{installations: installs(
		gh.Org("dfds"), gh.Org("dfds-internal"), gh.Org("Excluded"),
		gh.Enterprise("dfds"), gh.Enterprise("someone-else"),
	)}
	s := newFakeStore()
	s.scopes[gh.Org("gone")] = store.ScopeState{Scope: gh.Org("gone"), Active: true, CopilotEnabled: true}
	c := newCollectors(t, g, s)
	c.cfg.OrgExclude = []string{"excluded"}

	out, err := c.Members(context.Background())
	if err != nil || len(out.Errors) != 0 || out.Scopes != 2 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	active, _ := s.ActiveScopes(context.Background())
	var got []string
	for _, a := range active {
		got = append(got, fmt.Sprintf("%s#%d", a.Scope, a.InstallationID))
	}
	want := []string{"enterprise:dfds#103", "org:dfds#100", "org:dfds-internal#101"}
	if !slices.Equal(got, want) {
		t.Errorf("active scopes = %v, want %v", got, want)
	}
	if s.scopes[gh.Org("gone")].Active {
		t.Error("vanished org is still active")
	}
	if g.called("members Excluded") {
		t.Error("excluded org was enumerated")
	}
}

func TestMembersAddsEnterpriseWithoutInstallation(t *testing.T) {
	g := &fakeGitHub{installations: installs(gh.Org("dfds"))}
	s := newFakeStore()
	if _, err := newCollectors(t, g, s).Members(context.Background()); err != nil {
		t.Fatal(err)
	}
	ent, ok := s.scopes[gh.Enterprise("dfds")]
	if !ok || !ent.Active || ent.InstallationID != 0 {
		t.Fatalf("enterprise scope = %+v, present=%v", ent, ok)
	}
}

func TestMembersOrgErrorKeepsItsRows(t *testing.T) {
	g := &fakeGitHub{
		installations: installs(gh.Org("dfds"), gh.Org("dfds-internal")),
		members: map[string][]gh.Member{
			"dfds-internal": {{UserID: 1, Login: "a", VerifiedEmails: []string{"a@dfds.com"}}},
		},
		memberErr: map[string]error{"dfds": errors.New("graphql: boom")},
	}
	s := newFakeStore()
	old := []store.OrgMember{{UserID: 9, Login: "old"}}
	s.members["dfds"] = old

	out, err := newCollectors(t, g, s).Members(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := classify(out, err); status != store.RunPartial {
		t.Errorf("status = %s, want partial (out=%+v)", status, out)
	}
	if len(s.replaced) != 1 || !slices.Equal(s.replaced[0], []string{"dfds-internal"}) {
		t.Errorf("replaced orgs = %v, want only dfds-internal", s.replaced)
	}
	if fmt.Sprint(s.members["dfds"]) != fmt.Sprint(old) {
		t.Errorf("dfds rows changed: %+v", s.members["dfds"])
	}
	if out.Items != 1 {
		t.Errorf("items = %d", out.Items)
	}
}

func TestMembersAllOrgsFailSkipsWrite(t *testing.T) {
	g := &fakeGitHub{
		installations: installs(gh.Org("dfds")),
		memberErr:     map[string]error{"dfds": errors.New("boom")},
	}
	s := newFakeStore()
	out, err := newCollectors(t, g, s).Members(context.Background())
	if status, _ := classify(out, err); status != store.RunError {
		t.Errorf("status = %s, want error", status)
	}
	if len(s.replaced) != 0 {
		t.Errorf("ReplaceOrgMembers called with %v", s.replaced)
	}
}

func TestMembersInstallationsErrorFailsRun(t *testing.T) {
	g := &fakeGitHub{installErr: errors.New("401 bad JWT")}
	s := newFakeStore()
	s.scopes[gh.Org("dfds")] = store.ScopeState{Scope: gh.Org("dfds"), Active: true}
	if _, err := newCollectors(t, g, s).Members(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if !s.scopes[gh.Org("dfds")].Active {
		t.Error("scopes were deactivated after a failed installation listing")
	}
}

func TestToOrgMembersEmails(t *testing.T) {
	got := toOrgMembers([]gh.Member{
		{UserID: 1, Login: "a", Email: "A@dfds.com", VerifiedEmails: []string{"a@dfds.com", " A@DFDS.com", "a2@dfds.com"}},
		{UserID: 2, Login: "b", Email: "b@gmail.com"},
		{UserID: 3, Login: "c", Email: "C@Partner.DFDS.com"},
		{UserID: 4, Login: "d"},
	})
	want := [][]store.Email{
		{{Address: "a@dfds.com", Normalized: "a@dfds.com", Source: "verified"}, {Address: "a2@dfds.com", Normalized: "a2@dfds.com", Source: "verified"}},
		nil,
		{{Address: "C@Partner.DFDS.com", Normalized: "c@partner.dfds.com", Source: "profile"}},
		nil,
	}
	for i := range want {
		if fmt.Sprint(got[i].Emails) != fmt.Sprint(want[i]) {
			t.Errorf("member %d emails = %+v, want %+v", i, got[i].Emails, want[i])
		}
	}
}

func seatList(n int) []gh.Seat {
	var out []gh.Seat
	for i := range n {
		out = append(out, gh.Seat{UserID: int64(i + 1), Login: fmt.Sprintf("u%d", i)})
	}
	return out
}

func seatsStore(scopes ...gh.Scope) *fakeStore {
	s := newFakeStore()
	for _, sc := range scopes {
		s.scopes[sc] = store.ScopeState{Scope: sc, Active: true, CopilotEnabled: true}
	}
	return s
}

func TestSeatsSavesOrgAndEnterprise(t *testing.T) {
	g := &fakeGitHub{
		pat:     true,
		billing: map[string]*gh.CopilotBilling{"dfds": {SeatBreakdown: gh.SeatBreakdown{Total: 2}}},
		seats:   map[string][]gh.Seat{"org:dfds": seatList(2), "enterprise:dfds": seatList(3)},
	}
	s := seatsStore(gh.Org("dfds"), gh.Enterprise("dfds"))
	out, err := newCollectors(t, g, s).Seats(context.Background())
	if err != nil || len(out.Errors) != 0 || out.Items != 5 || out.Scopes != 2 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(s.seats[gh.Org("dfds")]) != 2 || len(s.seats[gh.Enterprise("dfds")]) != 3 || s.billing["dfds"] == nil {
		t.Errorf("stored seats=%v billing=%v", s.seats, s.billing)
	}
}

func TestSeatsLoadsInstallationsOnce(t *testing.T) {
	count := func(g *fakeGitHub) int {
		g.mu.Lock()
		defer g.mu.Unlock()
		n := 0
		for _, c := range g.calls {
			if c == "installations" {
				n++
			}
		}
		return n
	}
	ctx := context.Background()

	// Seats before any members run loads installations itself, once.
	g := &fakeGitHub{installErr: errors.New("dial timeout")}
	c := newCollectors(t, g, seatsStore(gh.Enterprise("dfds")))
	if _, err := c.Seats(ctx); err == nil {
		t.Fatal("seats ran without installations")
	}
	g.installErr = nil
	for range 2 {
		if _, err := c.Seats(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(g); n != 2 {
		t.Errorf("installations listed %d times, want 2 (one failed, one ok)", n)
	}

	// After a members run, seats does not list them again.
	g = &fakeGitHub{}
	c = newCollectors(t, g, seatsStore(gh.Enterprise("dfds")))
	if _, err := c.Members(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Seats(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(g); n != 1 {
		t.Errorf("installations listed %d times, want 1", n)
	}
}

func TestSeatsEmptyListingGuard(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scope     gh.Scope
		billTotal int
		wantSave  bool
	}{
		{"org billing says seats exist", gh.Org("dfds"), 4, false},
		{"org billing confirms zero", gh.Org("dfds"), 0, true},
		{"enterprise has no billing total", gh.Enterprise("dfds"), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &fakeGitHub{
				pat:     true,
				billing: map[string]*gh.CopilotBilling{"dfds": {SeatBreakdown: gh.SeatBreakdown{Total: tc.billTotal}}},
				seats:   map[string][]gh.Seat{},
			}
			s := seatsStore(tc.scope)
			s.seats[tc.scope] = seatList(4)
			out, err := newCollectors(t, g, s).Seats(context.Background())
			if err != nil || len(out.Errors) != 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if saved := s.seatSaves == 1; saved != tc.wantSave {
				t.Fatalf("saved = %v, want %v", saved, tc.wantSave)
			}
			if !tc.wantSave && len(s.seats[tc.scope]) != 4 {
				t.Errorf("stored seats were replaced: %v", s.seats[tc.scope])
			}
		})
	}
}

func TestSeatsCopilot404DisablesAndReenables(t *testing.T) {
	g := &fakeGitHub{billingErr: map[string]error{"dfds-al": fmt.Errorf("wrapped: %w", notFound)}}
	s := seatsStore(gh.Org("dfds-al"))
	c := newCollectors(t, g, s)

	out, err := c.Seats(context.Background())
	if err != nil || len(out.Errors) != 0 {
		t.Fatalf("a 404 is not a failure: out=%+v err=%v", out, err)
	}
	if s.scopes[gh.Org("dfds-al")].CopilotEnabled {
		t.Error("copilot_enabled still true after 404")
	}
	if g.called("seats org:dfds-al") {
		t.Error("seats fetched for a Copilot-less org")
	}

	g.billingErr = nil
	g.billing = map[string]*gh.CopilotBilling{"dfds-al": {SeatBreakdown: gh.SeatBreakdown{Total: 1}}}
	g.seats = map[string][]gh.Seat{"org:dfds-al": seatList(1)}
	if _, err := c.Seats(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.scopes[gh.Org("dfds-al")].CopilotEnabled || len(s.seats[gh.Org("dfds-al")]) != 1 {
		t.Errorf("not re-enabled: %+v seats=%v", s.scopes[gh.Org("dfds-al")], s.seats)
	}
}

func TestSeatsWithoutPATSkipsEnterprise(t *testing.T) {
	g := &fakeGitHub{
		billing: map[string]*gh.CopilotBilling{"dfds": {SeatBreakdown: gh.SeatBreakdown{Total: 1}}},
		seats:   map[string][]gh.Seat{"org:dfds": seatList(1)},
	}
	s := seatsStore(gh.Org("dfds"), gh.Enterprise("dfds"))
	out, err := newCollectors(t, g, s).Seats(context.Background())
	if err != nil || len(out.Errors) != 0 || out.Scopes != 1 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if g.called("seats enterprise:dfds") {
		t.Error("enterprise seats requested without a PAT")
	}
}

func TestSeatsScopeErrorIsPartial(t *testing.T) {
	g := &fakeGitHub{
		pat:     true,
		billing: map[string]*gh.CopilotBilling{"dfds": {SeatBreakdown: gh.SeatBreakdown{Total: 1}}},
		seats:   map[string][]gh.Seat{"org:dfds": seatList(1)},
		seatErr: map[string]error{"enterprise:dfds": errors.New("502")},
	}
	s := seatsStore(gh.Org("dfds"), gh.Enterprise("dfds"))
	out, err := newCollectors(t, g, s).Seats(context.Background())
	status, text := classify(out, err)
	if status != store.RunPartial || text != "enterprise:dfds: seats: 502" {
		t.Fatalf("status=%s text=%q", status, text)
	}
}

func TestMergeSeats(t *testing.T) {
	at := func(h int) *time.Time {
		v := time.Date(2026, 9, 20, h, 0, 0, 0, time.UTC)
		return &v
	}
	got := mergeSeats([]gh.Seat{
		{UserID: 1, OrgName: "dfds-internal", LastActivityAt: at(9), CreatedAt: at(1)},
		{UserID: 2, OrgName: "dfds"},
		{UserID: 1, OrgName: "dfds", LastActivityAt: at(12), CreatedAt: at(5)},
		{UserID: 2, OrgName: "dfds-internal"},
		{UserID: 3, OrgName: "dfds-internal"},
		{UserID: 3, OrgName: "dfds", LastActivityAt: at(3)},
	})
	var summary []string
	for _, s := range got {
		summary = append(summary, fmt.Sprintf("%d:%s", s.UserID, s.OrgName))
	}
	// 1: later activity wins; 2: no activity, lower org name; 3: any activity beats none.
	if want := []string{"1:dfds", "2:dfds", "3:dfds"}; !slices.Equal(summary, want) {
		t.Errorf("merged = %v, want %v", summary, want)
	}
	if !got[0].CreatedAt.Equal(*at(1)) {
		t.Errorf("CreatedAt = %s, want the earliest of the group", got[0].CreatedAt)
	}
}

func TestSeatsMergesEnterpriseDuplicates(t *testing.T) {
	dup := append(seatList(2), gh.Seat{UserID: 1, Login: "u0", OrgName: "dfds-internal"})
	g := &fakeGitHub{pat: true, seats: map[string][]gh.Seat{"enterprise:dfds": dup}}
	s := seatsStore(gh.Enterprise("dfds"))
	out, err := newCollectors(t, g, s).Seats(context.Background())
	if err != nil || out.Items != 2 || len(s.seats[gh.Enterprise("dfds")]) != 2 {
		t.Fatalf("out=%+v err=%v stored=%v", out, err, s.seats)
	}
}

func TestRetentionCutoff(t *testing.T) {
	s := newFakeStore()
	s.pruned = 7
	out, err := newCollectors(t, &fakeGitHub{}, s).Retention(context.Background())
	if err != nil || out.Items != 7 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if want := time.Date(2025, 8, 19, 0, 0, 0, 0, time.UTC); !s.cutoff.Equal(want) {
		t.Errorf("cutoff = %s, want %s", s.cutoff, want)
	}
}
