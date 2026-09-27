// Package identity maps GitHub logins to DFDS email addresses.
package identity

import "strings"

// IsDfdsTenantEmail accepts dfds.com and dfds.onmicrosoft.com, including their
// subdomains, case-insensitively. Leading-dot suffixes enforce label boundaries.
func IsDfdsTenantEmail(email string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	domain := strings.ToLower(strings.TrimSpace(email[at+1:]))
	if domain == "" {
		return false
	}
	switch {
	case domain == "dfds.com" || strings.HasSuffix(domain, ".dfds.com"):
		return true
	case domain == "dfds.onmicrosoft.com" || strings.HasSuffix(domain, ".dfds.onmicrosoft.com"):
		return true
	default:
		return false
	}
}

// NormalizeEmail is the lookup key for github_user_emails.email_normalized.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
