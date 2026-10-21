package caldav

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/emersion/go-ical"
	"github.com/mniehe/davkit/internal"
)

// limitRecurrenceCalendar returns cal without the overridden recurrence
// instances that impact the window in neither their current nor their original
// time (RFC 4791 §9.6.6). Everything else — masters, their recurrence rules,
// VTIMEZONEs, non-recurring components — is kept as stored. The input is never
// mutated: a Backend commonly returns pointers into its own cache.
func limitRecurrenceCalendar(cal *ical.Calendar, window *calendarTimeWindow) (*ical.Calendar, error) {
	// Overlap is judged on times resolved to UTC; resolveObjectTimes clones,
	// preserving child order, so resolved children align with cal's by index.
	resolved, err := resolveObjectTimes(cal, nil)
	if err != nil {
		return nil, err
	}

	// An override's original instance takes its shape from the master of the
	// same UID, so the masters are indexed before the overrides are judged.
	masters := make(map[string]*ical.Component)
	for _, child := range resolved.Children {
		if child.Name == ical.CompTimezone || child.Props.Get(ical.PropRecurrenceID) != nil {
			continue
		}
		masters[propValue(child, ical.PropUID)] = child
	}
	periods := newPeriodIndex()

	root := cloneComponent(cal.Component)
	root.Children = nil
	out := &ical.Calendar{Component: root}
	for i, child := range cal.Children {
		if child.Props.Get(ical.PropRecurrenceID) == nil {
			out.Children = append(out.Children, child)
			continue
		}
		impacts, err := overrideImpacts(resolved.Children[i], masters, periods, window)
		if err != nil {
			return nil, err
		}
		if impacts {
			out.Children = append(out.Children, child)
		}
	}
	return out, nil
}

// overrideImpacts reports whether an overridden instance impacts the window in
// either its current or its original time. RFC 4791 §9.6.6 requires both tests
// to use the logic CALDAV:time-range is defined with, which §9.9 makes
// dependent on the component's own type: a VTODO is judged on DUE and DURATION
// as much as on DTSTART, and a VEVENT alone occupies an interval.
func overrideImpacts(ov *ical.Component, masters map[string]*ical.Component, periods *periodIndex, window *calendarTimeWindow) (bool, error) {
	current, err := compOverlaps(window.Start, window.End, ov)
	if err != nil {
		return false, fmt.Errorf("caldav: stored override holds an unreadable interval: %w", err)
	}
	if current {
		return true, nil
	}

	ridProp := ov.Props.Get(ical.PropRecurrenceID)
	rid, err := ridProp.DateTime(time.UTC)
	if err != nil {
		return false, fmt.Errorf("caldav: stored override holds an unreadable RECURRENCE-ID: %w", err)
	}
	// RANGE=THISANDFUTURE redefines every instance from the RECURRENCE-ID on,
	// and one moving them earlier puts them as early as its own DTSTART, so
	// the override matters whenever the window reaches past the earlier of the
	// two. Retention is judged without walking the series.
	if isRanged(ridProp) {
		from, reachErr := rangedReach(ov, rid)
		if reachErr != nil {
			return false, reachErr
		}
		if from.Before(window.End) {
			return true, nil
		}
	}

	// The original instance is the master's occurrence at the RECURRENCE-ID,
	// measured as matching measures it. Without a master — a detached instance
	// stored on its own — the override's own shape is carried back to that
	// time instead.
	named := occurrence{start: rid, date: isDate(ridProp)}
	master, ok := masters[propValue(ov, ical.PropUID)]
	if !ok {
		return detachedImpacts(ov, named, periods, window)
	}

	// The interval is read first: the master's extra dates are read against
	// its DTSTART, which would otherwise fail there as an unreadable RDATE.
	extent, err := extentOf(master, time.UTC)
	if err != nil {
		return false, internal.HTTPErrorf(http.StatusForbidden, "caldav: cannot limit recurrence set: invalid component interval")
	}
	occ, err := originalOccurrence(master, named, periods)
	if err != nil {
		return false, err
	}
	return extent.overlaps(window.Start, window.End, occ), nil
}

// rangedReach is the earliest time a ranged override can put an instance:
// its RECURRENCE-ID, or its DTSTART when that is earlier.
func rangedReach(ov *ical.Component, rid time.Time) (time.Time, error) {
	start, ok, err := optDateTime(ov, ical.PropDateTimeStart, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("caldav: stored override holds an unreadable DTSTART: %w", err)
	}
	if ok && start.Before(rid) {
		return start, nil
	}
	return rid, nil
}

func detachedImpacts(ov *ical.Component, named occurrence, periods *periodIndex, window *calendarTimeWindow) (bool, error) {
	occ, err := originalOccurrence(ov, named, periods)
	if err != nil {
		return false, err
	}
	original, err := recurrenceInstance(ov, occ)
	if err != nil {
		return false, err
	}
	return compOverlaps(window.Start, window.End, original)
}

// originalOccurrence is the occurrence a RECURRENCE-ID names, ending where the
// RDATE period that starts there ends when shape lists one.
func originalOccurrence(shape *ical.Component, named occurrence, periods *periodIndex) (occurrence, error) {
	occ, found, err := periods.at(shape, named.start)
	if errors.Is(err, errTooManyRecurrenceDates) {
		return occurrence{}, expansionOverflow()
	}
	if err != nil {
		return occurrence{}, fmt.Errorf("caldav: stored master holds an unreadable RDATE or EXDATE: %w", err)
	}
	if !found {
		return named, nil
	}
	return occ, nil
}
