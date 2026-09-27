package domain

import "time"

// TimeLayout is the API's timestamp format: ISO 8601, milliseconds, Z suffix.
const TimeLayout = "2006-01-02T15:04:05.000Z"

// FormatTime renders t in TimeLayout. pgx returns times in the process's local
// zone, so t is converted to UTC first; without that, the literal Z in the
// layout would label a local time as UTC.
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeLayout)
}
