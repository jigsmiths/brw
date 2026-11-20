package cask

import (
	"time"

	"github.com/jigsmiths/brw/internal/api"
)

// DeprecationReasons are brew's words for the reasons it gives as symbols.
var DeprecationReasons = map[string]string{
	"discontinued":             "is discontinued upstream",
	"moved_to_mas":             "is now exclusively distributed on the Mac App Store",
	"no_longer_available":      "is no longer available upstream",
	"no_longer_meets_criteria": "no longer meets the criteria for acceptable casks",
	"unmaintained":             "is not maintained upstream",
	"fails_gatekeeper_check":   "does not pass the macOS Gatekeeper check",
	"unreachable":              "is no longer reliably reachable upstream",
}

// deprecation is brew's DeprecateDisable.message for a cask, or "" when it is
// neither deprecated nor disabled. Unlike brew info, it gives the estimated
// disable date even when that has passed.
func deprecation(c api.Cask, now time.Time) string {
	var kind, reason, formula, cask string
	switch {
	case c.Deprecated:
		kind, reason = "deprecated", c.DeprecationReason
		formula, cask = c.DeprecationReplacementFormula, c.DeprecationReplacementCask
	case c.Disabled:
		kind, reason = "disabled", c.DisableReason
		formula, cask = c.DisableReplacementFormula, c.DisableReplacementCask
	default:
		return ""
	}
	if r, ok := DeprecationReasons[reason]; ok {
		reason = r
	}
	msg := kind + "!"
	if reason != "" {
		msg = kind + " because it " + reason + "!"
	}
	date := c.DisableDate
	if date == "" && c.DeprecationDate != "" {
		if d, err := time.Parse(time.DateOnly, c.DeprecationDate); err == nil {
			next := time.Date(d.Year()+1, d.Month(), 1, 0, 0, 0, 0, time.UTC)
			last := next.AddDate(0, 1, -1).Day()
			date = time.Date(next.Year(), next.Month(), min(d.Day(), last), 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
		}
	}
	if date != "" {
		if date < now.Format(time.DateOnly) {
			msg += " It was disabled on " + date + "."
		} else {
			msg += " It will be disabled on " + date + "."
		}
	}
	replacement := ""
	switch {
	case formula != "" && formula == cask:
		replacement = formula
	case formula != "":
		replacement = "--formula " + formula
	case cask != "":
		replacement = "--cask " + cask
	}
	if replacement != "" {
		msg += "\nReplacement:\n  brew install " + replacement + "\n"
	}
	return msg
}
