package api

import (
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"time"
)

// fsSub narrows an embedded FS to a subdirectory.
func fsSub(f fs.FS, dir string) (fs.FS, error) {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		return nil, fmt.Errorf("sub filesystem %s: %w", dir, err)
	}
	return sub, nil
}

// templateFuncs are the helpers available inside the embedded templates.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"pct": func(f float64) int {
			return int(math.Round(f * 100))
		},
		"ago": humanizeAgo,
		"ts": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Local().Format("2006-01-02 15:04:05")
		},
		"deref": func(t *time.Time) time.Time {
			if t == nil {
				return time.Time{}
			}
			return *t
		},
		"truncate": func(n int, s string) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "…"
		},
		"yesno": func(b bool) string {
			if b {
				return "yes"
			}
			return "no"
		},
	}
}

// humanizeAgo renders a timestamp as a short relative duration. It accepts
// both time.Time and *time.Time so templates can pass nullable columns like a
// device's last-seen directly.
func humanizeAgo(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x == nil {
			return "never"
		}
		t = *x
	default:
		return "never"
	}

	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
