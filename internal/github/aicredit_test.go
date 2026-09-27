package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func statusBody(t *testing.T, f *fakeGitHub, status string) []byte {
	b, err := json.Marshal(map[string]any{"id": "r1", "status": status, "download_urls": []string{f.blobURL("credits")}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAICreditReport(t *testing.T) {
	env := newEnv(t, testPAT)
	base := "/enterprises/team-1/settings/billing/reports"
	env.fake.handle("POST "+base, serveBytes(http.StatusAccepted, []byte(`{"id":"r1","status":"processing"}`)))
	env.fake.handle("GET "+base+"/r1", sequence(
		serveBytes(http.StatusNotFound, []byte(`{"message":"Not Found"}`)),
		serveBytes(http.StatusOK, statusBody(t, env.fake, "processing")),
		serveBytes(http.StatusOK, statusBody(t, env.fake, "completed")),
	))
	env.fake.handle("GET /blob/credits", serveBytes(http.StatusOK, fixture(t, "ai_credit.csv")))

	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	rows, err := env.client.AICreditReport(context.Background(), testEnterprise, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 {
		t.Fatalf("rows = %d, want 6", len(rows))
	}

	post := env.fake.requests(base)[0]
	var req map[string]any
	if err := json.Unmarshal(post.Body, &req); err != nil {
		t.Fatal(err)
	}
	if req["report_type"] != "ai_credit" || req["start_date"] != "2026-09-01" || req["end_date"] != "2026-09-21" || req["send_email"] != false {
		t.Errorf("create body = %s", post.Body)
	}
	// The 404 poll is eventual consistency, not a refusal: no PAT retry.
	if a := env.fake.auths(base + "/r1"); !slices.Equal(a, []string{"app:693811", "app:693811", "app:693811"}) {
		t.Errorf("poll auths = %v", a)
	}
	if s := env.sleeps.all(); !slices.Equal(s, []time.Duration{reportPollInterval, reportPollInterval}) {
		t.Errorf("sleeps = %v", s)
	}
}

func TestAICreditReportFailed(t *testing.T) {
	env := newEnv(t, "")
	base := "/enterprises/team-1/settings/billing/reports"
	env.fake.handle("POST "+base, serveBytes(http.StatusAccepted, []byte(`{"id":"r1"}`)))
	env.fake.handle("GET "+base+"/r1", serveBytes(http.StatusOK, []byte(`{"id":"r1","status":"failed"}`)))
	_, err := env.client.AICreditReport(context.Background(), testEnterprise, time.Now(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestAICreditReportTimesOut(t *testing.T) {
	env := newEnv(t, "")
	env.client.pollMaxAttempts = 3
	base := "/enterprises/team-1/settings/billing/reports"
	env.fake.handle("POST "+base, serveBytes(http.StatusAccepted, []byte(`{"id":"r1"}`)))
	env.fake.handle("GET "+base+"/r1", serveBytes(http.StatusOK, []byte(`{"id":"r1","status":"processing"}`)))
	_, err := env.client.AICreditReport(context.Background(), testEnterprise, time.Now(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "did not complete within 3 polls") {
		t.Fatalf("err = %v", err)
	}
}

func TestAICreditExpiredLinkRepolls(t *testing.T) {
	env := newEnv(t, "")
	base := "/enterprises/team-1/settings/billing/reports"
	env.fake.handle("POST "+base, serveBytes(http.StatusAccepted, []byte(`{"id":"r1"}`)))
	env.fake.handle("GET "+base+"/r1", serveBytes(http.StatusOK, statusBody(t, env.fake, "completed")))
	env.fake.handle("GET /blob/credits", sequence(
		serveBytes(http.StatusForbidden, nil),
		serveBytes(http.StatusOK, fixture(t, "ai_credit.csv")),
	))
	rows, err := env.client.AICreditReport(context.Background(), testEnterprise, time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 || len(env.fake.requests(base+"/r1")) != 2 {
		t.Fatalf("rows = %d, polls = %d", len(rows), len(env.fake.requests(base+"/r1")))
	}
}

func TestParseAICreditCSVFixture(t *testing.T) {
	b := fixture(t, "ai_credit.csv")
	if !bytes.HasPrefix(b, utf8BOM) {
		t.Fatal("fixture lost its BOM; the test would not cover BOM stripping")
	}
	rows, err := ParseAICreditCSV(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	r := rows[0]
	want := AICreditRow{
		Date: "2032-04-30", Username: "user-r", Product: "copilot", SKU: "copilot_ai_credit", Model: "Code Review model",
		Quantity: 1309.3658, UnitType: "ai-credits", AppliedCostPerQuantity: 0.1157, GrossAmount: 151.4936, NetAmount: 151.4936,
		TotalMonthlyQuota: 4614, Organization: "org-2", Repository: "repo-5",
		InputTokens: 331.9756, OutputTokens: 62154.9946, CacheReadTokens: 78616.8434,
	}
	if r != want {
		t.Fatalf("row = %+v\nwant  %+v", r, want)
	}
}

func TestParseAICreditCSVErrors(t *testing.T) {
	for name, in := range map[string]string{
		"missing column": "date,username,product,sku,model\n2026-09-21,a,copilot,s,m\n",
		"bad number":     "date,username,product,sku,model,quantity\n2026-09-21,a,copilot,s,m,lots\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAICreditCSV(strings.NewReader(in)); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	rows, err := ParseAICreditCSV(strings.NewReader(""))
	if err != nil || rows != nil {
		t.Fatalf("empty csv: rows=%v err=%v", rows, err)
	}
	// Extra and reordered columns are tolerated.
	rows, err = ParseAICreditCSV(strings.NewReader("quantity,model,sku,product,username,date,new_col\n1.5,m,s,copilot,a,2026-09-21,x\n"))
	if err != nil || len(rows) != 1 || rows[0].Quantity != 1.5 || rows[0].Username != "a" {
		t.Fatalf("reordered: rows=%+v err=%v", rows, err)
	}
}

func TestStatusErrorIsNotFound(t *testing.T) {
	err := error(&StatusError{Status: http.StatusNotFound})
	if !errors.Is(err, ErrNotFound) {
		t.Error("404 should match ErrNotFound")
	}
	if errors.Is(&StatusError{Status: http.StatusForbidden}, ErrNotFound) {
		t.Error("403 should not match ErrNotFound")
	}
}
