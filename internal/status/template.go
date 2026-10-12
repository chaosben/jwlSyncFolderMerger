package status

import (
	_ "embed"
	"html/template"
	"time"
)

//go:embed status.html.tmpl
var pageSource string

var page = template.Must(template.New("status").Funcs(template.FuncMap{
	"when": func(t time.Time) string {
		if t.IsZero() {
			return "–"
		}
		return t.Local().Format("2006-01-02 15:04:05 -07:00")
	},
	"iso":    func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"symbol": symbol,
	"outcome": func(o string) string {
		switch o {
		case KindMerged:
			return "merged, restore current.jwlibrary on the devices"
		case KindUnchanged:
			return "new backups, nothing changed"
		case "nothing-to-do":
			return "no new backups"
		case KindGuard:
			return "aborted by the safety check"
		case KindError:
			return "failed"
		}
		return o
	},
}).Parse(pageSource))

// view adds derived values for the template.
type view struct {
	*state
	Data template.JS
}

// Level is the overall state shown in the banner: ok, warning or error.
func (v view) Level() string {
	r := v.LastRun
	switch {
	case r == nil:
		return "warning"
	case r.Outcome == KindError || r.Outcome == KindGuard:
		return "error"
	case r.Warnings > 0 || r.Pending > 0:
		return "warning"
	}
	return "ok"
}

// Headline is the banner text.
func (v view) Headline() string {
	r := v.LastRun
	switch {
	case r == nil:
		return "No merge run yet"
	case r.Outcome == KindGuard:
		return "Last run aborted by the safety check – nothing was changed"
	case r.Outcome == KindError:
		return "Last run failed"
	case r.Pending > 0:
		return "Waiting for backups that are still being synchronised"
	case r.Warnings > 0:
		return "Last run finished with warnings"
	}
	return "Everything is up to date"
}

// symbol is the icon of a log entry kind.
func symbol(kind string) string {
	switch kind {
	case KindMerged:
		return "✔"
	case KindUnchanged:
		return "="
	case KindGuard:
		return "⛔"
	case KindError:
		return "✖"
	case KindWarning:
		return "⚠"
	case KindUpdate:
		return "⬆"
	case KindStart:
		return "▶"
	case KindStop:
		return "■"
	}
	return "•"
}

// RestoredText explains Device.Restored.
func (d Device) RestoredText() string {
	switch d.Restored {
	case "current":
		return "yes"
	case "older":
		return "older version – restore current.jwlibrary"
	case "never":
		return "never"
	}
	return "–"
}
