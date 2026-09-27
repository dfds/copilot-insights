// Package report maps Copilot usage metrics report rows onto the copilot_*
// tables. Known fields fill wide columns; every other numeric field becomes a
// long-format Fact, so fields GitHub adds later are kept without a schema
// change. The raw row is stored alongside either way.
package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	gh "go.dfds.cloud/copilot-insights/internal/github"
)

const (
	BreakdownPRCommentType     = "pr_comment_type"
	BreakdownFeatureEngagement = "feature_engagement"
	// BreakdownExtra holds top-level numeric fields that have no column.
	BreakdownExtra = "extra"
)

// Fact is one numeric value of a breakdown. Key2 is "" for single-key
// breakdowns; both keys are "" for object breakdowns such as totals_by_cli.
// Zero values are not emitted (about two thirds of the user facts are zero),
// so a missing metric reads as 0; the raw row keeps the original.
type Fact struct {
	Breakdown string
	Key1      string
	Key2      string
	Metric    string
	Value     float64
}

type colKind int

const (
	colInt colKind = iota
	colFloat
	colBool
	colText
)

type column struct {
	Name string
	path []string
	kind colKind
}

func intCols(names ...string) []column {
	out := make([]column, len(names))
	for i, n := range names {
		out[i] = column{Name: n, path: []string{n}}
	}
	return out
}

// Pull request counters use a "pr_" prefix in storage.
func prCols() []column {
	var out []column
	for _, n := range []string{
		"total_created", "total_reviewed", "total_merged", "total_created_by_copilot",
		"total_reviewed_by_copilot", "total_merged_created_by_copilot", "total_merged_reviewed_by_copilot",
		"total_suggestions", "total_applied_suggestions", "total_copilot_suggestions",
		"total_copilot_applied_suggestions",
	} {
		out = append(out, column{Name: "pr_" + n, path: []string{"pull_requests", n}})
	}
	for _, n := range []string{
		"median_minutes_to_merge", "median_minutes_to_merge_copilot_authored",
		"median_minutes_to_merge_copilot_reviewed",
	} {
		out = append(out, column{Name: "pr_" + n, path: []string{"pull_requests", n}, kind: colFloat})
	}
	return out
}

var activityCols = intCols(
	"user_initiated_interaction_count", "code_generation_activity_count", "code_acceptance_activity_count",
	"loc_suggested_to_add_sum", "loc_suggested_to_delete_sum", "loc_added_sum", "loc_deleted_sum",
)

var dailyCols = slices.Concat(intCols(
	"daily_active_users", "weekly_active_users", "monthly_active_users",
	"monthly_active_chat_users", "monthly_active_agent_users",
	"daily_active_cli_users", "weekly_active_cli_users", "monthly_active_cli_users",
	"daily_active_copilot_app_users", "weekly_active_copilot_app_users", "monthly_active_copilot_app_users",
	"daily_active_copilot_cloud_agent_users", "weekly_active_copilot_cloud_agent_users",
	"monthly_active_copilot_cloud_agent_users",
	"daily_active_copilot_code_review_users", "weekly_active_copilot_code_review_users",
	"monthly_active_copilot_code_review_users",
	"daily_passive_copilot_code_review_users", "weekly_passive_copilot_code_review_users",
	"monthly_passive_copilot_code_review_users",
	"daily_active_vscode_agent_users", "weekly_active_vscode_agent_users", "monthly_active_vscode_agent_users",
), activityCols, prCols())

var userCols = func() []column {
	var out []column
	for _, n := range []string{
		"used_agent", "used_chat", "used_cli", "used_copilot_app", "used_copilot_cloud_agent",
		"used_copilot_coding_agent", "used_copilot_code_review_active", "used_copilot_code_review_passive",
		"used_vscode_agent",
	} {
		out = append(out, column{Name: n, path: []string{n}, kind: colBool})
	}
	out = append(out,
		column{Name: "ai_credits_used", path: []string{"ai_credits_used"}, kind: colFloat},
		column{Name: "ai_adoption_phase", path: []string{"ai_adoption_phase", "phase"}, kind: colText},
		column{Name: "ai_adoption_phase_number", path: []string{"ai_adoption_phase", "phase_number"}},
	)
	return append(out, activityCols...)
}()

var repoCols = prCols()

func names(cols []column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

func namesOf(cols []column, keep func(column) bool) []string {
	var out []string
	for _, c := range cols {
		if keep(c) {
			out = append(out, c.Name)
		}
	}
	return out
}

// additive holds the integer counters and float credits that may be summed
// across days; active-user counts, medians and the adoption phase may not.
func additive(c column) bool {
	switch {
	case c.kind == colBool, c.kind == colText, strings.HasPrefix(c.Name, "pr_median_"),
		strings.HasSuffix(c.Name, "_users"), c.Name == "ai_adoption_phase_number":
		return false
	}
	return true
}

// Column order must match the corresponding Values slices. Sum and Flag select
// fields for range aggregation.
var (
	DailyColumns = names(dailyCols)
	UserColumns  = names(userCols)
	RepoColumns  = names(repoCols)

	DailySumColumns = namesOf(dailyCols, additive)
	UserSumColumns  = namesOf(userCols, additive)
	RepoSumColumns  = namesOf(repoCols, additive)
	UserFlagColumns = namesOf(userCols, func(c column) bool { return c.kind == colBool })
)

// Daily is one organization-1-day or enterprise-1-day row.
type Daily struct {
	Values []any // aligned with DailyColumns; nil where the report omits the field
	Facts  []Fact
	Raw    json.RawMessage
}

// UserDay is one users-1-day row.
type UserDay struct {
	UserID    int64
	Login     string
	Values    []any // aligned with UserColumns
	IDEs      []string
	Languages []string
	Models    []string
	Facts     []Fact
	Raw       json.RawMessage
}

// RepoDay is one repos-1-day row.
type RepoDay struct {
	RepoID         int64
	OrganizationID string
	Owner          string
	Name           string
	Visibility     string
	Values         []any // aligned with RepoColumns
	Raw            json.RawMessage
}

// TeamMember is one user-teams-1-day row.
type TeamMember struct {
	TeamID int64
	UserID int64
	Slug   string
	Login  string
}

// Parsed is a report mapped onto rows. Exactly one field is set, matching
// Type and Window: Daily (summary, 1-day), Users, Repos, Teams, or
// Engagement (summary, 28-day).
type Parsed struct {
	Type       gh.ReportType
	Window     gh.Window
	Daily      *Daily
	Users      []UserDay
	Repos      []RepoDay
	Teams      []TeamMember
	Engagement []Fact
	// EndDay is report_end_day of a 28-day report (YYYY-MM-DD).
	EndDay string
}

// Parse maps a downloaded report. Of the 28-day reports only the summary is
// supported, for its copilot_feature_engagement block.
func Parse(t gh.ReportType, w gh.Window, rows []json.RawMessage) (*Parsed, error) {
	p := &Parsed{Type: t, Window: w}
	if w == gh.Window28Day {
		if t != gh.ReportSummary {
			return nil, fmt.Errorf("28-day %s reports are not collected", t)
		}
		if len(rows) != 1 {
			return nil, fmt.Errorf("28-day report: expected 1 document, got %d", len(rows))
		}
		m, err := decode(rows[0])
		if err != nil {
			return nil, err
		}
		p.EndDay, _ = m["report_end_day"].(string)
		if p.EndDay == "" {
			return nil, errors.New("28-day report: report_end_day missing")
		}
		p.Engagement = engagementFacts(m)
		return p, nil
	}

	for i, raw := range rows {
		m, err := decode(raw)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i+1, err)
		}
		switch t {
		case gh.ReportSummary:
			if p.Daily != nil {
				return nil, fmt.Errorf("%s 1-day report: expected 1 row, got %d", t, len(rows))
			}
			p.Daily = &Daily{Values: values(m, dailyCols), Facts: dailyFacts(m), Raw: raw}
		case gh.ReportUsers:
			u, err := parseUser(m, raw)
			if err != nil {
				return nil, fmt.Errorf("row %d: %w", i+1, err)
			}
			p.Users = append(p.Users, u)
		case gh.ReportRepos:
			r, err := parseRepo(m, raw)
			if err != nil {
				return nil, fmt.Errorf("row %d: %w", i+1, err)
			}
			p.Repos = append(p.Repos, r)
		case gh.ReportUserTeams:
			tm, err := parseTeamMember(m)
			if err != nil {
				return nil, fmt.Errorf("row %d: %w", i+1, err)
			}
			p.Teams = append(p.Teams, tm)
		default:
			return nil, fmt.Errorf("unknown report type %q", t)
		}
	}
	return p, nil
}

func decode(raw json.RawMessage) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("row is not a JSON object")
	}
	return m, nil
}

func number(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

func requiredID(m map[string]any, key string) (int64, error) {
	f, ok := number(m[key])
	if !ok || f != math.Trunc(f) {
		return 0, fmt.Errorf("%s missing or not an integer", key)
	}
	return int64(f), nil
}

func str(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	}
	return ""
}

func lookup(m map[string]any, path []string) any {
	var v any = m
	for _, p := range path {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = obj[p]
	}
	return v
}

// Missing, null and mistyped report fields remain nil.
func values(m map[string]any, cols []column) []any {
	out := make([]any, len(cols))
	for i, c := range cols {
		v := lookup(m, c.path)
		switch c.kind {
		case colBool:
			if b, ok := v.(bool); ok {
				out[i] = b
			}
		case colText:
			if s, ok := v.(string); ok {
				out[i] = s
			}
		case colFloat:
			if f, ok := number(v); ok {
				out[i] = f
			}
		default:
			if f, ok := number(v); ok {
				out[i] = int64(math.Round(f))
			}
		}
	}
	return out
}

// breakdownKeys names the key fields of known totals_by_* lists. Lists not
// named here are keyed on their string fields in name order.
var breakdownKeys = map[string][]string{
	"ide":               {"ide"},
	"feature":           {"feature"},
	"language_feature":  {"language", "feature"},
	"language_model":    {"language", "model"},
	"model_feature":     {"model", "feature"},
	"ai_adoption_phase": {"phase"},
}

// notMetrics are numeric fields that identify a row rather than measure it.
var notMetrics = map[string]bool{"user_id": true, "repo_id": true, "team_id": true}

func totalsFacts(m map[string]any) []Fact {
	var out []Fact
	for _, k := range sortedKeys(m) {
		name, ok := strings.CutPrefix(k, "totals_by_")
		if !ok {
			continue
		}
		out = append(out, breakdownFacts(name, m[k], breakdownKeys[name])...)
	}
	return out
}

// breakdownFacts handles a list of keyed objects or a single object.
func breakdownFacts(name string, v any, keys []string) []Fact {
	var out []Fact
	switch x := v.(type) {
	case []any:
		for _, el := range x {
			obj, ok := el.(map[string]any)
			if !ok {
				continue
			}
			k1, k2, used := elementKeys(obj, keys)
			flatten(obj, "", used, func(metric string, val float64) {
				out = append(out, Fact{Breakdown: name, Key1: k1, Key2: k2, Metric: metric, Value: val})
			})
		}
	case map[string]any:
		flatten(x, "", nil, func(metric string, val float64) {
			out = append(out, Fact{Breakdown: name, Metric: metric, Value: val})
		})
	}
	return out
}

func elementKeys(obj map[string]any, keys []string) (k1, k2 string, used map[string]bool) {
	if keys == nil {
		for _, k := range sortedKeys(obj) {
			if _, ok := obj[k].(string); ok {
				keys = append(keys, k)
			}
		}
	}
	used = map[string]bool{}
	var parts []string
	for _, k := range keys {
		used[k] = true
		parts = append(parts, str(obj, k))
	}
	if len(parts) > 0 {
		k1 = parts[0]
	}
	if len(parts) > 1 {
		k2 = strings.Join(parts[1:], "/")
	}
	return k1, k2, used
}

// flatten calls emit for each non-zero numeric field of obj, descending into
// nested objects with dotted names. Strings, booleans and lists are skipped.
func flatten(obj map[string]any, prefix string, skip map[string]bool, emit func(string, float64)) {
	for _, k := range sortedKeys(obj) {
		if skip[k] || (prefix == "" && notMetrics[k]) {
			continue
		}
		switch v := obj[k].(type) {
		case json.Number:
			if f, ok := number(v); ok && f != 0 {
				emit(prefix+k, f)
			}
		case map[string]any:
			flatten(v, prefix+k+".", nil, emit)
		}
	}
}

// extraFacts keeps top-level and pull_requests numeric fields that have no
// column in cols.
func extraFacts(m map[string]any, cols []column) []Fact {
	has := map[string]bool{}
	for _, c := range cols {
		has[strings.Join(c.path, ".")] = true
	}
	var out []Fact
	for _, k := range sortedKeys(m) {
		if f, ok := number(m[k]); ok && f != 0 && !has[k] && !notMetrics[k] {
			out = append(out, Fact{Breakdown: BreakdownExtra, Metric: k, Value: f})
		}
	}
	if pr, ok := m["pull_requests"].(map[string]any); ok {
		for _, k := range sortedKeys(pr) {
			if f, ok := number(pr[k]); ok && f != 0 && !has["pull_requests."+k] {
				out = append(out, Fact{Breakdown: BreakdownExtra, Metric: "pull_requests." + k, Value: f})
			}
		}
	}
	return out
}

func prCommentFacts(m map[string]any) []Fact {
	pr, ok := m["pull_requests"].(map[string]any)
	if !ok {
		return nil
	}
	return breakdownFacts(BreakdownPRCommentType, pr["copilot_suggestions_by_comment_type"], []string{"comment_type"})
}

func dailyFacts(m map[string]any) []Fact {
	return slices.Concat(totalsFacts(m), prCommentFacts(m), extraFacts(m, dailyCols))
}

// engagementFacts maps copilot_feature_engagement: its own numeric fields with
// empty keys, and totals_by_feature keyed on feature.
func engagementFacts(m map[string]any) []Fact {
	fe, ok := m["copilot_feature_engagement"].(map[string]any)
	if !ok {
		return nil
	}
	var out []Fact
	flatten(fe, "", nil, func(metric string, val float64) {
		out = append(out, Fact{Breakdown: BreakdownFeatureEngagement, Metric: metric, Value: val})
	})
	return append(out, breakdownFacts(BreakdownFeatureEngagement, fe["totals_by_feature"], []string{"feature"})...)
}

func parseUser(m map[string]any, raw json.RawMessage) (UserDay, error) {
	id, err := requiredID(m, "user_id")
	if err != nil {
		return UserDay{}, err
	}
	login := str(m, "user_login")
	if login == "" {
		return UserDay{}, errors.New("user_login missing")
	}
	return UserDay{
		UserID:    id,
		Login:     login,
		Values:    values(m, userCols),
		IDEs:      distinct(m, []string{"totals_by_ide"}, "ide"),
		Languages: distinct(m, []string{"totals_by_language_feature", "totals_by_language_model"}, "language"),
		Models:    distinct(m, []string{"totals_by_language_model", "totals_by_model_feature"}, "model"),
		Facts:     slices.Concat(totalsFacts(m), extraFacts(m, userCols)),
		Raw:       raw,
	}, nil
}

func distinct(m map[string]any, lists []string, field string) []string {
	seen := map[string]bool{}
	for _, l := range lists {
		arr, _ := m[l].([]any)
		for _, el := range arr {
			if obj, ok := el.(map[string]any); ok {
				if s := str(obj, field); s != "" {
					seen[s] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func parseRepo(m map[string]any, raw json.RawMessage) (RepoDay, error) {
	id, err := requiredID(m, "repo_id")
	if err != nil {
		return RepoDay{}, err
	}
	return RepoDay{
		RepoID:         id,
		OrganizationID: str(m, "organization_id"),
		Owner:          str(m, "repo_owner_name"),
		Name:           str(m, "repo_name"),
		Visibility:     str(m, "repo_visibility"),
		Values:         values(m, repoCols),
		Raw:            raw,
	}, nil
}

func parseTeamMember(m map[string]any) (TeamMember, error) {
	team, err := requiredID(m, "team_id")
	if err != nil {
		return TeamMember{}, err
	}
	user, err := requiredID(m, "user_id")
	if err != nil {
		return TeamMember{}, err
	}
	return TeamMember{TeamID: team, UserID: user, Slug: str(m, "slug"), Login: str(m, "user_login")}, nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
