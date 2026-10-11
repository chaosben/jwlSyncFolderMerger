// Package jwltime parses the various timestamp formats found in JW Library
// backups (Android writes "…Z", Windows "…+00:00", older data has fractions,
// offsets without colon or a space instead of "T").
package jwltime

import (
	"fmt"
	"strings"
	"time"
)

var layouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02T15:04:05Z0700",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02",
}

// Parse returns the instant described by s in UTC.
func Parse(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if len(s) > 10 && s[10] == ' ' {
		s = s[:10] + "T" + s[11:]
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", s)
}

// Canonical normalises s to "2006-01-02T15:04:05Z" so that equal instants in
// different notations compare equal. Unparseable input is returned unchanged.
func Canonical(s string) string {
	t, err := Parse(s)
	if err != nil {
		return s
	}
	return FormatDB(t)
}

// FormatDB formats t the way the Android app writes timestamps into the database.
func FormatDB(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}
