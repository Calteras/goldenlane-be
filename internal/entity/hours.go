package entity

import (
	"strconv"
	"strings"
	"time"
)

// minutesOf parses "HH:MM"; ok is false for anything else.
func minutesOf(hhmm string) (int, bool) {
	h, m, found := strings.Cut(hhmm, ":")
	if !found || len(h) != 2 || len(m) != 2 {
		return 0, false
	}

	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)

	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}

	return hh*60 + mm, true
}

// WithinHours reports whether now falls inside any weekly interval, in the given timezone.
// An unknown timezone falls back to UTC rather than failing a customer's search.
func WithinHours(hours []Hours, now time.Time, timezone string) bool {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}

	local := now.In(loc)
	day := int(local.Weekday())
	yesterday := (day + 6) % 7
	minutes := local.Hour()*60 + local.Minute()

	for _, h := range hours {
		opens, ok1 := minutesOf(h.OpensAt)
		closes, ok2 := minutesOf(h.ClosesAt)

		if !ok1 || !ok2 {
			continue
		}

		overnight := closes < opens

		switch {
		case h.DayOfWeek == day:
			if overnight && minutes >= opens || !overnight && minutes >= opens && minutes < closes {
				return true
			}
		case overnight && h.DayOfWeek == yesterday && minutes < closes:
			// The tail of yesterday's overnight interval.
			return true
		}
	}

	return false
}
