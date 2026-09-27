package store_test

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"go.dfds.cloud/copilot-insights/internal/dbtest"
	"go.dfds.cloud/copilot-insights/internal/store"
)

func TestOptionsDSN_EscapesCredentials(t *testing.T) {
	o := store.Options{Host: "db.local", Port: 5533, Name: "copilot_insights", User: "app", Password: "p@ss:w rd/", SSLMode: "disable"}
	u, err := url.Parse(o.DSN())
	if err != nil {
		t.Fatalf("DSN does not parse: %v", err)
	}
	if pw, _ := u.User.Password(); pw != o.Password {
		t.Errorf("password round-trip: got %q", pw)
	}
	if u.Host != "db.local:5533" || u.Path != "/copilot_insights" || u.Query().Get("sslmode") != "disable" {
		t.Errorf("unexpected DSN parts: %s", u.Redacted())
	}
}

var allTables = []string{
	"scopes", "collector_runs", "github_users", "github_user_emails", "copilot_report_raw",
	"copilot_daily", "copilot_daily_facts", "copilot_user_daily", "copilot_user_daily_facts",
	"copilot_repo_daily", "copilot_user_teams", "copilot_seats_current", "copilot_seats_snapshot",
	"copilot_billing_summary", "billing_usage", "ai_credit_usage", "premium_request_usage_legacy",
}

func tableCount(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	err := s.DB().QueryRow(
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = ANY($1)`,
		allTables).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrate_UpDownUp(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	s := store.New(db)

	applied, err := s.Migrate(ctx)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("expected migrations to apply on an empty schema")
	}
	if n := tableCount(t, s); n != len(allTables) {
		t.Fatalf("after up: %d of %d tables", n, len(allTables))
	}

	again, err := s.Migrate(ctx)
	if err != nil || len(again) != 0 {
		t.Fatalf("second up should be a no-op: applied=%v err=%v", again, err)
	}

	if err := store.MigrateDownAll(ctx, db); err != nil {
		t.Fatalf("down: %v", err)
	}
	if n := tableCount(t, s); n != 0 {
		t.Fatalf("after down: %d tables left", n)
	}

	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("up after down: %v", err)
	}
}

func TestReady(t *testing.T) {
	db := dbtest.Open(t)
	ctx := context.Background()
	s := store.New(db)

	if err := s.Ready(ctx); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("before migrate: got %v, want ErrNotReady", err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(ctx); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("no runs: got %v, want ErrNotReady", err)
	}

	id, err := s.StartRun(ctx, "seats")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(ctx); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("running only: got %v, want ErrNotReady", err)
	}
	if err := s.FinishRun(ctx, id, store.RunError, 0, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(ctx); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("failed run only: got %v, want ErrNotReady", err)
	}

	id, _ = s.StartRun(ctx, "seats")
	if err := s.FinishRun(ctx, id, store.RunPartial, 12, "org x: 500"); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(ctx); err != nil {
		t.Fatalf("after partial run: %v", err)
	}

	last, err := s.LastSuccess(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := last["seats"]; !ok || len(last) != 1 {
		t.Errorf("LastSuccess: %v", last)
	}
}

func TestFailStaleRuns(t *testing.T) {
	s := store.New(dbtest.OpenMigrated(t))
	ctx := context.Background()

	stale, _ := s.StartRun(ctx, "reports")
	done, _ := s.StartRun(ctx, "seats")
	if err := s.FinishRun(ctx, done, store.RunOK, 1, ""); err != nil {
		t.Fatal(err)
	}

	n, err := s.FailStaleRuns(ctx)
	if err != nil || n != 1 {
		t.Fatalf("FailStaleRuns: n=%d err=%v", n, err)
	}
	var status string
	if err := s.DB().QueryRow(`SELECT status FROM collector_runs WHERE id = $1`, stale).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "error" {
		t.Errorf("stale run status: %s", status)
	}
}
