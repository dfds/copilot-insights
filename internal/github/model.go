package github

import (
	"encoding/json"
	"time"
)

type ScopeKind string

const (
	ScopeOrg        ScopeKind = "org"
	ScopeEnterprise ScopeKind = "enterprise"
)

// Scope is an org or the enterprise. Name is the org login or enterprise slug.
type Scope struct {
	Kind ScopeKind
	Name string
}

func Org(name string) Scope        { return Scope{Kind: ScopeOrg, Name: name} }
func Enterprise(slug string) Scope { return Scope{Kind: ScopeEnterprise, Name: slug} }

func (s Scope) String() string { return string(s.Kind) + ":" + s.Name }

// Installation is an org or enterprise GitHub App installation.
type Installation struct {
	ID          int64
	Scope       Scope
	Permissions map[string]string
}

type installationPayload struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Slug  string `json:"slug"`
	} `json:"account"`
	TargetType  string            `json:"target_type"`
	Permissions map[string]string `json:"permissions"`
}

// Member is one org member from GraphQL membersWithRole. Email is the public
// profile email and is unverified; VerifiedEmails are the member's emails on
// the org's verified domains.
type Member struct {
	UserID         int64
	Login          string
	Name           string
	Email          string
	VerifiedEmails []string
}

// Seat is one Copilot seat assignment. OrgName is the org the seat is billed
// through; enterprise seat listings name it per seat.
type Seat struct {
	UserID                  int64
	Login                   string
	OrgName                 string
	AssigningTeamSlug       string
	PlanType                string
	CreatedAt               *time.Time
	UpdatedAt               *time.Time
	LastActivityAt          *time.Time
	LastAuthenticatedAt     *time.Time
	LastActivityEditor      string
	PendingCancellationDate string // YYYY-MM-DD, empty when none
}

type seatPayload struct {
	CreatedAt               *time.Time `json:"created_at"`
	UpdatedAt               *time.Time `json:"updated_at"`
	LastActivityAt          *time.Time `json:"last_activity_at"`
	LastAuthenticatedAt     *time.Time `json:"last_authenticated_at"`
	LastActivityEditor      *string    `json:"last_activity_editor"`
	PendingCancellationDate *string    `json:"pending_cancellation_date"`
	PlanType                string     `json:"plan_type"`
	Assignee                *struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"assignee"`
	AssigningTeam *struct {
		Slug string `json:"slug"`
	} `json:"assigning_team"`
	Organization *struct {
		Login string `json:"login"`
	} `json:"organization"`
}

type seatsPage struct {
	TotalSeats int           `json:"total_seats"`
	Seats      []seatPayload `json:"seats"`
}

// CopilotBilling is an org's Copilot seat summary and policy settings from
// GET /orgs/{org}/copilot/billing.
type CopilotBilling struct {
	SeatBreakdown         SeatBreakdown
	SeatManagementSetting string
	PlanType              string
	// Policies holds every other top-level field (public_code_suggestions,
	// ide_chat, cli, platform_chat, and any GitHub adds later) verbatim.
	Policies map[string]json.RawMessage
}

type SeatBreakdown struct {
	Total               int `json:"total"`
	AddedThisCycle      int `json:"added_this_cycle"`
	PendingInvitation   int `json:"pending_invitation"`
	PendingCancellation int `json:"pending_cancellation"`
	ActiveThisCycle     int `json:"active_this_cycle"`
	InactiveThisCycle   int `json:"inactive_this_cycle"`
}

type BillingUsageResponse struct {
	UsageItems []BillingUsageItem `json:"usageItems"`
}

type BillingUsageItem struct {
	Date             string  `json:"date"`
	Product          string  `json:"product"`
	SKU              string  `json:"sku"`
	Quantity         float64 `json:"quantity"`
	UnitType         string  `json:"unitType"`
	PricePerUnit     float64 `json:"pricePerUnit"`
	GrossAmount      float64 `json:"grossAmount"`
	DiscountAmount   float64 `json:"discountAmount"`
	NetAmount        float64 `json:"netAmount"`
	OrganizationName string  `json:"organizationName"`
	RepositoryName   string  `json:"repositoryName"`
}

type PremiumRequestUsage struct {
	TimePeriod struct {
		Year  int `json:"year"`
		Month int `json:"month"`
	} `json:"timePeriod"`
	Enterprise   string                    `json:"enterprise"`
	Organization string                    `json:"organization"`
	User         string                    `json:"user"`
	UsageItems   []PremiumRequestUsageItem `json:"usageItems"`
}

type PremiumRequestUsageItem struct {
	Product          string  `json:"product"`
	SKU              string  `json:"sku"`
	Model            string  `json:"model"`
	UnitType         string  `json:"unitType"`
	PricePerUnit     float64 `json:"pricePerUnit"`
	GrossQuantity    float64 `json:"grossQuantity"`
	GrossAmount      float64 `json:"grossAmount"`
	DiscountQuantity float64 `json:"discountQuantity"`
	DiscountAmount   float64 `json:"discountAmount"`
	NetQuantity      float64 `json:"netQuantity"`
	NetAmount        float64 `json:"netAmount"`
}

type billingReportCreateRequest struct {
	ReportType string `json:"report_type"`
	StartDate  string `json:"start_date"`
	EndDate    string `json:"end_date,omitempty"`
	SendEmail  bool   `json:"send_email"`
}

type billingReportStatus struct {
	ID           string   `json:"id"`
	Status       string   `json:"status"`
	DownloadURLs []string `json:"download_urls"`
}

// AICreditRow is one row of the ai_credit billing report CSV: one user, model,
// repository and cost centre on one day. Token columns are token counts.
type AICreditRow struct {
	Date                   string
	Username               string
	Product                string
	SKU                    string
	Model                  string
	Quantity               float64
	UnitType               string
	AppliedCostPerQuantity float64
	GrossAmount            float64
	DiscountAmount         float64
	NetAmount              float64
	TotalMonthlyQuota      float64
	Organization           string
	Repository             string
	CostCenterName         string
	AICQuantity            float64
	AICGrossAmount         float64
	InputTokens            float64
	OutputTokens           float64
	CacheReadTokens        float64
	CacheWriteTokens       float64
}
