package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ReportType is a Copilot usage metrics report independent of scope and
// window. ReportSummary is "organization-*" on orgs and "enterprise-*" on the
// enterprise.
type ReportType string

const (
	ReportSummary   ReportType = "summary"
	ReportUsers     ReportType = "users"
	ReportRepos     ReportType = "repos"
	ReportUserTeams ReportType = "user-teams"
)

type Window string

const (
	Window1Day  Window = "1-day"
	Window28Day Window = "28-day"
)

// APIName is the report's name in the URL, e.g. "organization-1-day".
func (t ReportType) APIName(kind ScopeKind, w Window) string {
	name := string(t)
	if t == ReportSummary {
		name = "organization"
		if kind == ScopeEnterprise {
			name = "enterprise"
		}
	}
	return name + "-" + string(w)
}

// Report is a downloaded usage metrics report. NoData is set when GitHub has
// nothing for the day: a 204, a JSON "" body, or an envelope without links.
// Rows holds one JSON document per NDJSON line or array element.
type Report struct {
	Name     string
	NoData   bool
	Day      string // 1-day reports
	StartDay string // 28-day reports
	EndDay   string
	Rows     []json.RawMessage
}

type reportEnvelope struct {
	DownloadLinks  []string `json:"download_links"`
	ReportDay      string   `json:"report_day"`
	ReportStartDay string   `json:"report_start_day"`
	ReportEndDay   string   `json:"report_end_day"`
}

// DailyReport fetches a 1-day report. GitHub serves days up to a year back
// and answers 400 for older ones.
func (c *Client) DailyReport(ctx context.Context, scope Scope, t ReportType, day time.Time) (*Report, error) {
	name := t.APIName(scope.Kind, Window1Day)
	path := fmt.Sprintf("%s/copilot/metrics/reports/%s?day=%s", scopeBase(scope), name, day.Format(time.DateOnly))
	return c.report(ctx, scope, name, path)
}

// LatestReport fetches the most recent 28-day report.
func (c *Client) LatestReport(ctx context.Context, scope Scope, t ReportType) (*Report, error) {
	name := t.APIName(scope.Kind, Window28Day)
	path := fmt.Sprintf("%s/copilot/metrics/reports/%s/latest", scopeBase(scope), name)
	return c.report(ctx, scope, name, path)
}

func (c *Client) report(ctx context.Context, scope Scope, name, path string) (*Report, error) {
	// An expired download link requires a fresh report envelope.
	for attempt := 0; ; attempt++ {
		r, err := c.call(ctx, scope, familyReports, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		out := &Report{Name: name}
		if r.status == http.StatusNoContent || isEmptyJSON(r.body) {
			out.NoData = true
			return out, nil
		}
		var env reportEnvelope
		if err := json.Unmarshal(r.body, &env); err != nil {
			return nil, fmt.Errorf("decode report envelope %s: %w", path, err)
		}
		out.Day, out.StartDay, out.EndDay = env.ReportDay, env.ReportStartDay, env.ReportEndDay
		if len(env.DownloadLinks) == 0 {
			out.NoData = true
			return out, nil
		}
		rows, err := c.downloadRows(ctx, env.DownloadLinks)
		if errors.Is(err, errLinkExpired) && attempt == 0 {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("report %s %s: %w", scope, name, err)
		}
		out.Rows = rows
		return out, nil
	}
}

func (c *Client) downloadRows(ctx context.Context, links []string) ([]json.RawMessage, error) {
	var rows []json.RawMessage
	for _, link := range links {
		b, err := c.fetchSigned(ctx, link)
		if err != nil {
			return nil, err
		}
		part, err := parseRows(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", redactURL(link), err)
		}
		rows = append(rows, part...)
	}
	return rows, nil
}

// isEmptyJSON matches the bodies GitHub sends for a day without data: empty,
// null, or the JSON string "".
func isEmptyJSON(b []byte) bool {
	t := bytes.TrimSpace(b)
	return len(t) == 0 || bytes.Equal(t, []byte(`""`)) || bytes.Equal(t, []byte("null"))
}

// parseRows accepts a JSON array, a single JSON document, or NDJSON.
func parseRows(b []byte) ([]json.RawMessage, error) {
	t := bytes.TrimSpace(b)
	if len(t) == 0 {
		return nil, nil
	}
	if t[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(t, &arr); err != nil {
			return nil, fmt.Errorf("decode JSON array: %w", err)
		}
		return arr, nil
	}
	if json.Valid(t) {
		return []json.RawMessage{json.RawMessage(t)}, nil
	}
	var rows []json.RawMessage
	for i, line := range bytes.Split(t, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			return nil, fmt.Errorf("NDJSON line %d is not valid JSON", i+1)
		}
		rows = append(rows, json.RawMessage(line))
	}
	return rows, nil
}
