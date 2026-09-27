package github

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// The billing report is rendered asynchronously. GitHub suggests polling every
// 30s; 15s returns sooner when a report finishes mid-interval. Month-to-date
// reports can take about 40 minutes, hence 160 attempts.
const (
	reportPollInterval    = 15 * time.Second
	reportPollMaxAttempts = 160
)

// AICreditReport requests the ai_credit billing report for start..end
// (inclusive days), waits for it, and returns its rows.
func (c *Client) AICreditReport(ctx context.Context, enterprise string, start, end time.Time) ([]AICreditRow, error) {
	scope := Enterprise(enterprise)
	id, err := c.createBillingReport(ctx, scope, "ai_credit", start, end)
	if err != nil {
		return nil, err
	}
	c.logger.Info("ai_credit report queued", zap.String("id", id),
		zap.String("start", start.Format(time.DateOnly)), zap.String("end", end.Format(time.DateOnly)))

	urls, err := c.awaitBillingReport(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	rows, err := c.aiCreditRows(ctx, urls)
	if errors.Is(err, errLinkExpired) {
		if urls, err = c.awaitBillingReport(ctx, scope, id); err != nil {
			return nil, err
		}
		rows, err = c.aiCreditRows(ctx, urls)
	}
	if err != nil {
		return nil, fmt.Errorf("ai_credit report %s: %w", id, err)
	}
	return rows, nil
}

func (c *Client) createBillingReport(ctx context.Context, scope Scope, reportType string, start, end time.Time) (string, error) {
	body, err := json.Marshal(billingReportCreateRequest{
		ReportType: reportType,
		StartDate:  start.Format(time.DateOnly),
		EndDate:    end.Format(time.DateOnly),
	})
	if err != nil {
		return "", err
	}
	r, err := c.call(ctx, scope, familyBillingReports, http.MethodPost, scopeBase(scope)+"/settings/billing/reports", body)
	if err != nil {
		return "", fmt.Errorf("create %s report: %w", reportType, err)
	}
	var st billingReportStatus
	if err := json.Unmarshal(r.body, &st); err != nil {
		return "", fmt.Errorf("decode %s report create response: %w", reportType, err)
	}
	if st.ID == "" {
		return "", fmt.Errorf("create %s report: response has no id", reportType)
	}
	return st.ID, nil
}

func (c *Client) awaitBillingReport(ctx context.Context, scope Scope, id string) ([]string, error) {
	path := scopeBase(scope) + "/settings/billing/reports/" + id
	for attempt := 1; attempt <= c.pollMaxAttempts; attempt++ {
		var st billingReportStatus
		r, err := c.callWith(ctx, scope, familyBillingReports, http.MethodGet, path, nil, false)
		if err == nil {
			if err := json.Unmarshal(r.body, &st); err != nil {
				return nil, fmt.Errorf("decode billing report %s status: %w", id, err)
			}
		}
		switch {
		// A new report ID can briefly return 404 while GitHub propagates it.
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return nil, fmt.Errorf("poll billing report %s: %w", id, err)
		case st.Status == "completed":
			c.logger.Info("billing report ready", zap.String("id", id), zap.Int("polls", attempt), zap.Int("files", len(st.DownloadURLs)))
			return st.DownloadURLs, nil
		case st.Status == "failed":
			return nil, fmt.Errorf("billing report %s failed", id)
		case st.Status != "processing" && st.Status != "queued" && st.Status != "":
			return nil, fmt.Errorf("billing report %s has unexpected status %q", id, st.Status)
		}
		if err := c.sleep(ctx, c.pollInterval); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("billing report %s did not complete within %d polls", id, c.pollMaxAttempts)
}

func (c *Client) aiCreditRows(ctx context.Context, urls []string) ([]AICreditRow, error) {
	var all []AICreditRow
	for _, u := range urls {
		b, err := c.fetchSigned(ctx, u)
		if err != nil {
			return nil, err
		}
		rows, err := ParseAICreditCSV(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", redactURL(u), err)
		}
		all = append(all, rows...)
	}
	return all, nil
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

var aiCreditRequired = []string{"date", "username", "product", "sku", "model", "quantity"}

// ParseAICreditCSV decodes an ai_credit report. GitHub prefixes the file with
// a UTF-8 BOM, which would otherwise become part of the first column name.
// Columns are matched by name; a missing required column or a malformed
// number is an error rather than a zero.
func ParseAICreditCSV(r io.Reader) ([]AICreditRow, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimPrefix(b, utf8BOM)
	cr := csv.NewReader(bytes.NewReader(b))
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true // model names contain bare quotes
	header, err := cr.Read()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read csv header: %w", err)
	}
	idx := make(map[string]int, len(header))
	for i, h := range header {
		idx[strings.TrimSpace(h)] = i
	}
	for _, col := range aiCreditRequired {
		if _, ok := idx[col]; !ok {
			return nil, fmt.Errorf("csv has no %q column", col)
		}
	}

	var rows []AICreditRow
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read csv line %d: %w", line, err)
		}
		p := csvRow{rec: rec, idx: idx}
		row := AICreditRow{
			Date:                   p.str("date"),
			Username:               p.str("username"),
			Product:                p.str("product"),
			SKU:                    p.str("sku"),
			Model:                  p.str("model"),
			Quantity:               p.num("quantity"),
			UnitType:               p.str("unit_type"),
			AppliedCostPerQuantity: p.num("applied_cost_per_quantity"),
			GrossAmount:            p.num("gross_amount"),
			DiscountAmount:         p.num("discount_amount"),
			NetAmount:              p.num("net_amount"),
			TotalMonthlyQuota:      p.num("total_monthly_quota"),
			Organization:           p.str("organization"),
			Repository:             p.str("repository"),
			CostCenterName:         p.str("cost_center_name"),
			AICQuantity:            p.num("aic_quantity"),
			AICGrossAmount:         p.num("aic_gross_amount"),
			InputTokens:            p.num("input"),
			OutputTokens:           p.num("output"),
			CacheReadTokens:        p.num("cache_read"),
			CacheWriteTokens:       p.num("cache_write"),
		}
		if p.err != nil {
			return nil, fmt.Errorf("csv line %d: %w", line, p.err)
		}
		rows = append(rows, row)
	}
}

type csvRow struct {
	rec []string
	idx map[string]int
	err error
}

func (p *csvRow) str(col string) string {
	if i, ok := p.idx[col]; ok && i < len(p.rec) {
		return p.rec[i]
	}
	return ""
}

func (p *csvRow) num(col string) float64 {
	s := strings.TrimSpace(p.str(col))
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil && p.err == nil {
		p.err = fmt.Errorf("column %s: %q is not a number", col, s)
	}
	return v
}
