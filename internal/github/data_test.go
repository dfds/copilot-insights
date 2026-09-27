package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestOrgMembersPagesAndFilters(t *testing.T) {
	env := newEnv(t, "")
	var cursors []any
	env.fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		body := env.fake.requests("/graphql")
		if err := json.Unmarshal(body[len(body)-1].Body, &req); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(req.Query, "databaseId") || req.Variables["org"] != "org-2" {
			t.Errorf("unexpected query %q vars %v", req.Query, req.Variables)
		}
		cursors = append(cursors, req.Variables["cursor"])
		if req.Variables["cursor"] == nil {
			w.Write([]byte(`{"data":{"organization":{"membersWithRole":{
			  "pageInfo":{"hasNextPage":true,"endCursor":"c1"},
			  "nodes":[
			    {"databaseId":11,"login":"user-g","name":"User G","email":"","organizationVerifiedDomainEmails":["user-g@example.com","g2@example.com"]},
			    {"databaseId":12,"login":"ci[bot]","name":null,"email":null,"organizationVerifiedDomainEmails":[]}]}}}}`))
			return
		}
		w.Write([]byte(`{"data":{"organization":{"membersWithRole":{
		  "pageInfo":{"hasNextPage":false,"endCursor":"c2"},
		  "nodes":[{"databaseId":13,"login":"user-i","name":null,"email":"user-i@example.com","organizationVerifiedDomainEmails":[]}]}}}}`))
	})
	got, err := env.client.OrgMembers(context.Background(), "org-2")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cursors, []any{nil, "c1"}) {
		t.Errorf("cursors = %v", cursors)
	}
	want := []Member{
		{UserID: 11, Login: "user-g", Name: "User G", VerifiedEmails: []string{"user-g@example.com", "g2@example.com"}},
		{UserID: 13, Login: "user-i", Email: "user-i@example.com", VerifiedEmails: []string{}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("members = %+v\nwant      %+v", got, want)
	}
	if a := env.fake.auths("/graphql"); a[0] != "app:480569" {
		t.Errorf("auth = %v", a)
	}
}

func TestOrgMembersGraphQLErrorFailsWholeCall(t *testing.T) {
	env := newEnv(t, "")
	env.fake.handle("POST /graphql", serveBytes(http.StatusOK, []byte(`{"data":{"organization":{"membersWithRole":{
	  "pageInfo":{"hasNextPage":false},"nodes":[{"databaseId":1,"login":"a","organizationVerifiedDomainEmails":[]}]}}},
	  "errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`)))
	_, err := env.client.OrgMembers(context.Background(), "org-1")
	if err == nil || !strings.Contains(err.Error(), "Resource not accessible") {
		t.Fatalf("err = %v, want the GraphQL error", err)
	}
}

func TestOrgMembersFixtureDecodes(t *testing.T) {
	// This fixture has no databaseId, so every node is skipped while its
	// payload shape is still decoded.
	env := newEnv(t, "")
	env.fake.handle("POST /graphql", serveBytes(http.StatusOK, fixture(t, "graphql_members.json")))
	got, err := env.client.OrgMembers(context.Background(), "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("members without databaseId were kept: %+v", got)
	}
}

func TestOrgSeats(t *testing.T) {
	env := newEnv(t, testPAT)
	env.fake.handle("GET /orgs/org-2/copilot/billing/seats", serveBytes(http.StatusOK, fixture(t, "org_copilot_seats.json")))
	seats, err := env.client.Seats(context.Background(), Org("org-2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(seats) != 3 {
		t.Fatalf("seats = %d", len(seats))
	}
	s := seats[0]
	if s.UserID != 893733 || s.Login != "user-a" || s.OrgName != "org-2" || s.PlanType != "business" ||
		s.LastActivityAt == nil || s.CreatedAt == nil || s.PendingCancellationDate != "" ||
		!strings.HasPrefix(s.LastActivityEditor, "VS/") {
		t.Errorf("seat = %+v", s)
	}
	reqs := env.fake.requests("/orgs/org-2/copilot/billing/seats")
	if len(reqs) != 1 || reqs[0].Auth != "app:480569" || reqs[0].Query != "per_page=100&page=1" {
		t.Errorf("requests = %+v", reqs)
	}
}

func TestSeatsPaging(t *testing.T) {
	env := newEnv(t, "")
	env.fake.handle("GET /orgs/org-1/copilot/billing/seats", func(w http.ResponseWriter, r *http.Request) {
		n := 100
		if r.URL.Query().Get("page") == "2" {
			n = 5
		}
		var seats []string
		for i := range n {
			seats = append(seats, fmt.Sprintf(`{"assignee":{"id":%d,"login":"u%d"},"plan_type":"business"}`, len(r.URL.RawQuery)*1000+i, i))
		}
		fmt.Fprintf(w, `{"total_seats":105,"seats":[%s]}`, strings.Join(seats, ","))
	})
	seats, err := env.client.Seats(context.Background(), Org("org-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(seats) != 105 {
		t.Fatalf("seats = %d, want 105", len(seats))
	}
}

func TestEnterpriseSeatsNeedPAT(t *testing.T) {
	env := newEnv(t, testPAT)
	path := "/enterprises/team-1/copilot/billing/seats"
	env.fake.handle("GET "+path, serveBytes(http.StatusOK, fixture(t, "enterprise_copilot_seats.json")))
	seats, err := env.client.Seats(context.Background(), Enterprise(testEnterprise))
	if err != nil {
		t.Fatal(err)
	}
	if len(seats) != 3 || seats[0].OrgName != "org-1" || seats[0].Login != "user-d" {
		t.Fatalf("seats = %+v", seats)
	}
	if a := env.fake.auths(path); !slices.Equal(a, []string{"pat"}) {
		t.Errorf("auths = %v, want PAT only", a)
	}

	noPAT := newEnv(t, "")
	if _, err := noPAT.client.Seats(context.Background(), Enterprise(testEnterprise)); !errors.Is(err, ErrNoPAT) {
		t.Fatalf("err = %v, want ErrNoPAT", err)
	}
	if n := len(noPAT.fake.requests(path)); n != 0 {
		t.Errorf("sent %d requests without a PAT", n)
	}
}

func TestCopilotBilling(t *testing.T) {
	env := newEnv(t, "")
	env.fake.handle("GET /orgs/org-1/copilot/billing", serveBytes(http.StatusOK, fixture(t, "org_copilot_billing.json")))
	b, err := env.client.CopilotBilling(context.Background(), "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.SeatBreakdown.Total != 4 || b.SeatBreakdown.ActiveThisCycle != 4 || b.PlanType != "business" || b.SeatManagementSetting != "assign_selected" {
		t.Errorf("billing = %+v", b)
	}
	var keys []string
	for k := range b.Policies {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"cli", "ide_chat", "platform_chat", "public_code_suggestions"}) {
		t.Errorf("policy keys = %v", keys)
	}
}

func TestBillingUsage(t *testing.T) {
	env := newEnv(t, "")
	path := "/enterprises/team-1/settings/billing/usage"
	env.fake.handle("GET "+path, serveBytes(http.StatusOK, fixture(t, "enterprise_billing_usage.json")))
	u, err := env.client.BillingUsage(context.Background(), testEnterprise, 2026, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.UsageItems) == 0 {
		t.Fatal("no usage items")
	}
	it := u.UsageItems[0]
	if it.Date != "2031-08-11T07:13:29.379000Z" || it.Product != "copilot" || it.OrganizationName != "org-2" || it.Quantity == 0 {
		t.Errorf("item = %+v", it)
	}
	r := env.fake.requests(path)[0]
	if r.Query != "month=9&year=2026" || r.Auth != "app:693811" {
		t.Errorf("request = %+v", r)
	}
}

func TestPremiumRequestUsage(t *testing.T) {
	env := newEnv(t, "")
	entPath := "/enterprises/team-1/settings/billing/premium_request/usage"
	orgPath := "/organizations/org-2/settings/billing/premium_request/usage"
	env.fake.handle("GET "+entPath, serveBytes(http.StatusOK, fixture(t, "enterprise_premium_request_usage_user.json")))
	env.fake.handle("GET "+orgPath, serveBytes(http.StatusOK, fixture(t, "org_premium_request_usage.json")))

	u, err := env.client.PremiumRequestUsage(context.Background(), Enterprise(testEnterprise), PremiumRequestQuery{Year: 2026, Month: 3, User: "user-d"})
	if err != nil {
		t.Fatal(err)
	}
	if u.User != "user-d" || u.TimePeriod.Month != 7 || len(u.UsageItems) == 0 || u.UsageItems[0].GrossQuantity == 0 {
		t.Errorf("enterprise usage = %+v", u)
	}
	if q := env.fake.requests(entPath)[0].Query; q != "month=3&user=user-d&year=2026" {
		t.Errorf("query = %q", q)
	}

	o, err := env.client.PremiumRequestUsage(context.Background(), Org("org-2"), PremiumRequestQuery{Year: 2026, Month: 3})
	if err != nil {
		t.Fatal(err)
	}
	if o.Organization != "org-2" || len(o.UsageItems) == 0 {
		t.Errorf("org usage = %+v", o)
	}
	if a := env.fake.auths(orgPath); !slices.Equal(a, []string{"app:480569"}) {
		t.Errorf("org auths = %v", a)
	}
}
