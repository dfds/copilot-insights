package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setMinimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CPI_GITHUB_ENTERPRISE", "dfds")
	t.Setenv("CPI_GITHUB_APPID", "123")
	t.Setenv("CPI_GITHUB_APPPRIVATEKEY", "key")
	t.Setenv("CPI_DB_HOST", "localhost")
	t.Setenv("CPI_DB_USER", "postgres")
	t.Setenv("CPI_OIDC_ENABLED", "false")
}

func TestLoad_Defaults(t *testing.T) {
	setMinimalEnv(t)

	conf, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"LogLevel", conf.LogLevel, "info"},
		{"APIPort", conf.APIPort, 8080},
		{"GitHub.Concurrency", conf.GitHub.Concurrency, 4},
		{"GitHub.APIVersion", conf.GitHub.APIVersion, "2026-03-10"},
		{"GitHub.TimeoutMs", conf.GitHub.TimeoutMs, 60000},
		{"DB.Port", conf.DB.Port, 5432},
		{"DB.Name", conf.DB.Name, "copilot_insights"},
		{"DB.SSLMode", conf.DB.SSLMode, "require"},
		{"DB.MaxOpenConns", conf.DB.MaxOpenConns, 10},
		{"Collect.BillingInterval", conf.Collect.BillingInterval, time.Hour},
		{"Collect.AICreditInterval", conf.Collect.AICreditInterval, time.Hour},
		{"Collect.MembersInterval", conf.Collect.MembersInterval, 6 * time.Hour},
		{"Collect.SeatsInterval", conf.Collect.SeatsInterval, 6 * time.Hour},
		{"Collect.ReportsInterval", conf.Collect.ReportsInterval, 24 * time.Hour},
		{"Collect.BackfillDays", conf.Collect.BackfillDays, 90},
		{"Collect.ReportLagDays", conf.Collect.ReportLagDays, 1},
		{"Collect.BillingLagDays", conf.Collect.BillingLagDays, 0},
		{"Collect.TeamsEnabled", conf.Collect.TeamsEnabled, false},
		{"Collect.AICreditEnabled", conf.Collect.AICreditEnabled, true},
		{"Collect.RunOnStart", conf.Collect.RunOnStart, true},
		{"Retention.RawDays", conf.Retention.RawDays, 400},
		{"OIDC.RequiredRoles", strings.Join(conf.OIDC.RequiredRoles, ","), "copilot.read"},
		{"OIDC.AdminRoles", strings.Join(conf.OIDC.AdminRoles, ","), "copilot.admin"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoad_Overrides(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("CPI_APIPORT", "8081")
	t.Setenv("CPI_DB_PORT", "5533")
	t.Setenv("CPI_DB_SSLMODE", "disable")
	t.Setenv("CPI_COLLECT_REPORTSINTERVAL", "12h")
	t.Setenv("CPI_COLLECT_BACKFILLDAYS", "365")
	t.Setenv("CPI_GITHUB_ORGEXCLUDE", "dfds-al, ,dfds-data")

	conf, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conf.APIPort != 8081 || conf.DB.Port != 5533 || conf.DB.SSLMode != "disable" {
		t.Errorf("overrides not applied: %+v %+v", conf.APIPort, conf.DB)
	}
	if conf.Collect.ReportsInterval != 12*time.Hour || conf.Collect.BackfillDays != 365 {
		t.Errorf("collect overrides not applied: %+v", conf.Collect)
	}
	if got := strings.Join(conf.GitHub.OrgExclude, ","); got != "dfds-al,dfds-data" {
		t.Errorf("OrgExclude: got %q", got)
	}
}

func TestLoad_PrivateKeyPath(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("CPI_GITHUB_APPPRIVATEKEY", "")
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CPI_GITHUB_APPPRIVATEKEYPATH", path)

	conf, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conf.GitHub.AppPrivateKey != "from-file" {
		t.Errorf("key not read from path: %q", conf.GitHub.AppPrivateKey)
	}
}

func TestLoad_PrivateKeyNewlines(t *testing.T) {
	const pem = "-----BEGIN RSA PRIVATE KEY-----\nabc+/=\ndef\n-----END RSA PRIVATE KEY-----\n"
	cases := []struct {
		name string
		env  string
	}{
		{"multi-line", pem},
		{"escaped newlines", strings.ReplaceAll(pem, "\n", `\n`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setMinimalEnv(t)
			t.Setenv("CPI_GITHUB_APPPRIVATEKEY", c.env)

			conf, err := Load()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if conf.GitHub.AppPrivateKey != pem {
				t.Errorf("got %q, want %q", conf.GitHub.AppPrivateKey, pem)
			}
		})
	}
}

func TestLoad_Errors(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"no enterprise", map[string]string{"CPI_GITHUB_ENTERPRISE": ""}, "CPI_GITHUB_ENTERPRISE"},
		{"no app id", map[string]string{"CPI_GITHUB_APPID": ""}, "CPI_GITHUB_APPID"},
		{"no key", map[string]string{"CPI_GITHUB_APPPRIVATEKEY": ""}, "CPI_GITHUB_APPPRIVATEKEYPATH"},
		{"missing key file", map[string]string{"CPI_GITHUB_APPPRIVATEKEY": "", "CPI_GITHUB_APPPRIVATEKEYPATH": "/nonexistent.pem"}, "read CPI_GITHUB_APPPRIVATEKEYPATH"},
		{"no db host", map[string]string{"CPI_DB_HOST": ""}, "CPI_DB_HOST"},
		{"backfill too large", map[string]string{"CPI_COLLECT_BACKFILLDAYS": "366"}, "CPI_COLLECT_BACKFILLDAYS"},
		{"interval too short", map[string]string{"CPI_COLLECT_SEATSINTERVAL": "10s"}, "CPI_COLLECT_SEATSINTERVAL"},
		{"oidc without issuer", map[string]string{"CPI_OIDC_ENABLED": "true", "CPI_OIDC_AUDIENCE": "aud"}, "CPI_OIDC_ISSUERURL"},
		{"oidc without audience", map[string]string{"CPI_OIDC_ENABLED": "true", "CPI_OIDC_ISSUERURL": "https://issuer"}, "CPI_OIDC_AUDIENCE"},
		{"oidc with blank roles", map[string]string{"CPI_OIDC_ENABLED": "true", "CPI_OIDC_ISSUERURL": "https://issuer", "CPI_OIDC_AUDIENCE": "aud", "CPI_OIDC_ADMINROLES": " "}, "CPI_OIDC_ADMINROLES"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setMinimalEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not mention %q", err, c.wantErr)
			}
		})
	}
}
