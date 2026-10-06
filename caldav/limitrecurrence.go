package caldav

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"
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
	// so the override matters whenever the window reaches past that point.
	if strings.EqualFold(ridProp.Params.Get(ical.ParamRange), "THISANDFUTURE") && rid.Before(window.End) {
		return true, nil
	}

	// The original instance is the master materialised at the RECURRENCE-ID.
	// Without a master to take the shape from — a detached instance stored on
	// its own — the override's own shape is carried back to that time instead.
	shape := ov
	if master, ok := masters[propValue(ov, ical.PropUID)]; ok {
		shape = master
	}
	original, err := originalInstance(shape, rid, periods)
	if err != nil {
		return false, err
	}
	return compOverlaps(window.Start, window.End, original)
}

// originalInstance materialises shape at rid, with the end of the RDATE period
// that starts there when shape lists one.
func originalInstance(shape *ical.Component, rid time.Time, periods *periodIndex) (*ical.Component, error) {
	occ, found, err := periods.at(shape, rid)
	if errors.Is(err, errTooManyRecurrenceDates) {
		return nil, expansionOverflow()
	}
	if err != nil {
		return nil, fmt.Errorf("caldav: stored master holds an unreadable RDATE: %w", err)
	}
	if !found {
		occ = occurrence{start: rid}
	}
	return recurrenceInstance(shape, occ)
}
