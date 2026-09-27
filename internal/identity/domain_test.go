package identity

import "testing"

func TestIsDfdsTenantEmail(t *testing.T) {
	cases := []struct {
		email string
		want  bool
	}{
		{"alice@dfds.com", true},
		{"alice@DFDS.COM", true},
		{"bob@PARTNER.DFDS.COM", true},
		{"carol@dfds.onmicrosoft.com", true},
		{"dave@x.dfds.onmicrosoft.com", true},
		{"  eve@dfds.com  ", true}, // surrounding whitespace tolerated on the domain side
		{"mallory@notdfds.com", false},
		{"oscar@dfds.com.evil.com", false},
		{"trent@example.com", false},
		{"peggy@dfds.onmicrosoft.com.evil.com", false},
		{"", false},
		{"no-at-sign", false},
		{"trailing-at@", false},
	}
	for _, c := range cases {
		if got := IsDfdsTenantEmail(c.email); got != c.want {
			t.Errorf("IsDfdsTenantEmail(%q) = %v, want %v", c.email, got, c.want)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	if got := NormalizeEmail("  Alice.Smith@DFDS.com "); got != "alice.smith@dfds.com" {
		t.Errorf("NormalizeEmail = %q", got)
	}
}
