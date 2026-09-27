package domain_test

import (
	"testing"
	"time"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

func TestFormatTime(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+30*60)

	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{"UTC", time.Date(2026, 9, 26, 10, 0, 3, 120_000_000, time.UTC), "2026-09-26T10:00:03.120Z"},
		// 15:30:03 IST is 10:00:03 UTC; without .UTC() this would print 15:30:03.120Z.
		{"non-UTC zone converted", time.Date(2026, 9, 26, 15, 30, 3, 120_000_000, ist), "2026-09-26T10:00:03.120Z"},
		{"conversion crosses midnight", time.Date(2026, 9, 27, 2, 0, 0, 0, ist), "2026-09-26T20:30:00.000Z"},
		{"zero milliseconds padded", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "2026-01-02T03:04:05.000Z"},
		// Sub-millisecond digits are dropped; Postgres has already rounded to ms (timestamptz(3)).
		{"sub-millisecond dropped", time.Date(2026, 1, 2, 3, 4, 5, 999_999, time.UTC), "2026-01-02T03:04:05.000Z"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain.FormatTime(tt.in); got != tt.want {
				t.Fatalf("FormatTime = %q, want %q", got, tt.want)
			}
		})
	}
}
