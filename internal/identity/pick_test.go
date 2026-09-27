package identity

import (
	"slices"
	"testing"
	"time"
)

func at(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestPick(t *testing.T) {
	cases := []struct {
		name       string
		cands      []Candidate
		want       string
		wantOthers []string
	}{
		{"single", []Candidate{{Login: "a"}}, "a", nil},
		{"seated beats activity", []Candidate{
			{Login: "unseated", LastActivityAt: at("2026-09-22T00:00:00Z")},
			{Login: "seated", Seated: true},
		}, "seated", []string{"unseated"}},
		{"recent activity wins", []Candidate{
			{Login: "old", Seated: true, LastActivityAt: at("2026-01-01T00:00:00Z")},
			{Login: "new", Seated: true, LastActivityAt: at("2026-09-01T00:00:00Z")},
			{Login: "never", Seated: true},
		}, "new", []string{"old", "never"}},
		{"login order breaks ties", []Candidate{
			{Login: "Zed"}, {Login: "bob"}, {Login: "Alice"},
		}, "Alice", []string{"bob", "Zed"}},
		{"equal activity falls to login", []Candidate{
			{Login: "b", Seated: true, LastActivityAt: at("2026-09-01T00:00:00Z")},
			{Login: "a", Seated: true, LastActivityAt: at("2026-09-01T00:00:00Z")},
		}, "a", []string{"b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, others, ok := Pick(tc.cands)
			if !ok || got.Login != tc.want || !slices.Equal(others, tc.wantOthers) {
				t.Errorf("Pick = %q %v %v, want %q %v", got.Login, others, ok, tc.want, tc.wantOthers)
			}
		})
	}
	if _, _, ok := Pick(nil); ok {
		t.Error("Pick(nil) reported a match")
	}
}
