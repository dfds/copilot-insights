package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
)

// MaxBackfillDays is the oldest day the usage metrics reports serve; older
// days return 400 "Date must be within the last year".
const MaxBackfillDays = 365

// Config is populated from CPI_ environment variables.
type Config struct {
	LogLevel string `default:"info"`
	LogDebug bool
	// APIPort is the port the OIDC-protected REST API listens on. The Prometheus
	// metrics server (bootstrap) stays on :9090.
	APIPort int `default:"8080"`

	GitHub struct {
		Enterprise string
		AppID      int64
		// AppPrivateKey takes precedence over AppPrivateKeyPath. Load reads the
		// path into AppPrivateKey, so consumers only look at AppPrivateKey.
		AppPrivateKey     string
		AppPrivateKeyPath string
		// PAT is only used for enterprise Copilot seats, which no App permission
		// covers. Empty disables that collector.
		PAT         string
		OrgExclude  []string
		Concurrency int    `default:"4"`
		APIVersion  string `default:"2026-03-10"`
		TimeoutMs   int    `default:"60000"`
	}

	DB struct {
		Host         string
		Port         int    `default:"5432"`
		Name         string `default:"copilot_insights"`
		User         string
		Password     string
		SSLMode      string `default:"require"`
		MaxOpenConns int    `default:"10"`
	}

	Collect struct {
		BillingInterval  time.Duration `default:"1h"`
		AICreditInterval time.Duration `default:"1h"`
		BudgetsInterval  time.Duration `default:"1h"`
		MembersInterval  time.Duration `default:"6h"`
		SeatsInterval    time.Duration `default:"6h"`
		ReportsInterval  time.Duration `default:"24h"`
		BackfillDays     int           `default:"90"`
		ReportLagDays    int           `default:"1"`
		BillingLagDays   int           `default:"0"`
		TeamsEnabled     bool          `default:"false"`
		AICreditEnabled  bool          `default:"true"`
		// PremiumBackfill requests the retired premium request usage for
		// 2026-01..05 once per enterprise, org and member.
		PremiumBackfill bool `default:"true"`
		RunOnStart      bool `default:"true"`
	}

	Retention struct {
		// RawDays applies to copilot_report_raw only. Normalized tables are
		// never pruned.
		RawDays int `default:"400"`
	}

	OIDC struct {
		Enabled   bool `default:"true"`
		IssuerURL string
		Audience  string
		// Role values are compared case-sensitively against the token's roles
		// claim, so they must match the Entra app role values exactly.
		RequiredRoles []string `default:"copilot.read"`
		AdminRoles    []string `default:"copilot.admin"`
	}
}

const appConfPrefix = "CPI"

// Load applies defaults and validates CPI_ environment variables.
func Load() (Config, error) {
	var conf Config
	if err := envconfig.Process(appConfPrefix, &conf); err != nil {
		return conf, err
	}

	conf.GitHub.OrgExclude = trimEntries(conf.GitHub.OrgExclude)
	conf.OIDC.RequiredRoles = trimEntries(conf.OIDC.RequiredRoles)
	conf.OIDC.AdminRoles = trimEntries(conf.OIDC.AdminRoles)

	if conf.GitHub.Enterprise == "" {
		return conf, fmt.Errorf("CPI_GITHUB_ENTERPRISE must be set")
	}
	if conf.GitHub.AppID == 0 {
		return conf, fmt.Errorf("CPI_GITHUB_APPID must be set")
	}
	if conf.GitHub.AppPrivateKey == "" {
		if conf.GitHub.AppPrivateKeyPath == "" {
			return conf, fmt.Errorf("CPI_GITHUB_APPPRIVATEKEY or CPI_GITHUB_APPPRIVATEKEYPATH must be set")
		}
		key, err := os.ReadFile(conf.GitHub.AppPrivateKeyPath)
		if err != nil {
			return conf, fmt.Errorf("read CPI_GITHUB_APPPRIVATEKEYPATH: %w", err)
		}
		conf.GitHub.AppPrivateKey = string(key)
	}
	// A key stored on one line carries literal `\n` sequences. PEM has no
	// backslashes, so the replacement leaves a multi-line key unchanged.
	conf.GitHub.AppPrivateKey = strings.ReplaceAll(conf.GitHub.AppPrivateKey, `\n`, "\n")
	if conf.GitHub.Concurrency < 1 {
		return conf, fmt.Errorf("CPI_GITHUB_CONCURRENCY must be at least 1")
	}

	if conf.DB.Host == "" {
		return conf, fmt.Errorf("CPI_DB_HOST must be set")
	}
	if conf.DB.User == "" {
		return conf, fmt.Errorf("CPI_DB_USER must be set")
	}

	if conf.Collect.BackfillDays < 0 || conf.Collect.BackfillDays > MaxBackfillDays {
		return conf, fmt.Errorf("CPI_COLLECT_BACKFILLDAYS must be between 0 and %d", MaxBackfillDays)
	}
	if conf.Collect.ReportLagDays < 0 || conf.Collect.BillingLagDays < 0 {
		return conf, fmt.Errorf("CPI_COLLECT_REPORTLAGDAYS and CPI_COLLECT_BILLINGLAGDAYS must not be negative")
	}
	for name, d := range map[string]time.Duration{
		"BILLINGINTERVAL":  conf.Collect.BillingInterval,
		"AICREDITINTERVAL": conf.Collect.AICreditInterval,
		"BUDGETSINTERVAL":  conf.Collect.BudgetsInterval,
		"MEMBERSINTERVAL":  conf.Collect.MembersInterval,
		"SEATSINTERVAL":    conf.Collect.SeatsInterval,
		"REPORTSINTERVAL":  conf.Collect.ReportsInterval,
	} {
		if d < time.Minute {
			return conf, fmt.Errorf("CPI_COLLECT_%s must be at least 1m", name)
		}
	}

	if conf.OIDC.Enabled {
		if conf.OIDC.IssuerURL == "" {
			return conf, fmt.Errorf("CPI_OIDC_ISSUERURL must be set when OIDC is enabled")
		}
		if conf.OIDC.Audience == "" {
			return conf, fmt.Errorf("CPI_OIDC_AUDIENCE must be set when OIDC is enabled")
		}
		if len(conf.OIDC.RequiredRoles) == 0 || len(conf.OIDC.AdminRoles) == 0 {
			return conf, fmt.Errorf("CPI_OIDC_REQUIREDROLES and CPI_OIDC_ADMINROLES must not be empty when OIDC is enabled")
		}
	}

	return conf, nil
}

// trimEntries drops blank entries; envconfig turns an empty variable into a
// single empty element.
func trimEntries(values []string) []string {
	out := values[:0]
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
