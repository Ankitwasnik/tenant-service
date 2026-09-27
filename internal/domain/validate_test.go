package domain_test

import (
	"strings"
	"testing"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

func TestValidateSlug(t *testing.T) {
	tests := []struct {
		name  string
		slug  string
		valid bool
	}{
		{"length 2", "ab", false},
		{"length 3", "abc", true},
		{"length 28", "a" + strings.Repeat("b", 26) + "c", true},
		{"length 29", "a" + strings.Repeat("b", 27) + "c", false},
		{"empty", "", false},
		{"leading digit", "1abc", false},
		{"leading dash", "-abc", false},
		{"trailing dash", "abc-", false},
		{"trailing digit", "abc1", true},
		{"inner dash and digits", "acme-prod-01", true},
		{"consecutive dashes", "a--b", true}, // the pattern allows it, as k8s does
		{"uppercase", "Acme", false},
		{"uppercase inside", "acMe", false},
		{"underscore", "ac_me", false},
		{"dot", "ac.me", false},
		{"space", "ac me", false},
		{"non-ASCII letter", "acmé", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := domain.ValidateSlug(tt.slug)
			if (err == nil) != tt.valid {
				t.Fatalf("ValidateSlug(%q) = %v, want valid=%v", tt.slug, err, tt.valid)
			}
		})
	}
}

func TestValidateSlugMessages(t *testing.T) {
	if err := domain.ValidateSlug("ab"); err == nil || err.Error() != "must be 3-28 characters" {
		t.Errorf("too short: %v", err)
	}
	if err := domain.ValidateSlug("1abc"); err == nil || !strings.HasPrefix(err.Error(), "must start with a lowercase letter") {
		t.Errorf("bad format: %v", err)
	}
}

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{"plain", "Acme Corp", "Acme Corp", ""},
		{"trimmed", "  Acme Corp \t\n", "Acme Corp", ""},
		{"inner spaces kept", "Acme   Corp", "Acme   Corp", ""},
		{"empty", "", "", "must not be empty"},
		{"whitespace only", "   \t ", "", "must not be empty"},
		{"200 characters", strings.Repeat("a", 200), strings.Repeat("a", 200), ""},
		{"201 characters", strings.Repeat("a", 201), "", "must be at most 200 characters"},
		// Length is counted in characters: 200 two-byte runes are 400 bytes, and allowed.
		{"200 multi-byte characters", strings.Repeat("é", 200), strings.Repeat("é", 200), ""},
		{"201 multi-byte characters", strings.Repeat("é", 201), "", "must be at most 200 characters"},
		// Trimming happens before the length check.
		{"200 characters plus padding", " " + strings.Repeat("a", 200) + " ", strings.Repeat("a", 200), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := domain.NormalizeName(tt.in)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeName = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateCreate(t *testing.T) {
	t.Run("valid, name normalized", func(t *testing.T) {
		name, err := domain.ValidateCreate("acme", "  Acme Corp ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "Acme Corp" {
			t.Fatalf("name = %q", name)
		}
	})

	t.Run("every invalid field reported", func(t *testing.T) {
		_, err := domain.ValidateCreate("A", " ")

		de := asDomainError(t, err)
		if de.Code != domain.CodeValidation {
			t.Fatalf("code = %q", de.Code)
		}
		if de.Details["slug"] != "must be 3-28 characters" || de.Details["name"] != "must not be empty" {
			t.Fatalf("details = %v", de.Details)
		}
	})

	t.Run("one invalid field", func(t *testing.T) {
		_, err := domain.ValidateCreate("acme", "")

		de := asDomainError(t, err)
		if _, ok := de.Details["slug"]; ok || len(de.Details) != 1 {
			t.Fatalf("details = %v, want only name", de.Details)
		}
	})
}
