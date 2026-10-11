package caldav

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

// errTooManyRecurrenceDates is returned when a component lists more RDATE and
// EXDATE values than maxRecurrenceIterations allows a walk to take; each
// caller reports it as its own iteration overflow.
var errTooManyRecurrenceDates = errors.New("caldav: recurrence dates exceed the iteration cap")

const periodSeparator = "/"

// occurrence is one instance of a recurrence set. own marks an end stated by
// an RDATE period; any other occurrence takes its extent from the component.
// date marks a start written as a DATE, which need not match DTSTART's type
// when it comes from an RDATE.
type occurrence struct {
	start, end time.Time
	own, date  bool
}

// recurrence is a component's recurrence set per RFC 5545 §3.8.5: DTSTART and
// the RRULE's starts, plus every RDATE, minus every EXDATE. go-ical's
// RecurrenceSet serves only the RRULE half: it gives up on a component with no
// RRULE and cannot read an RDATE period.
type recurrence struct {
	starts func() func() (time.Time, bool)
	// allDay marks the starts as DATEs, as DTSTART is.
	allDay bool
	// dates are the RDATE occurrences, ascending, one per start, with EXDATE
	// already applied.
	dates []occurrence
}

// recurrenceOf reads comp's recurrence set in loc, or returns nil when comp
// has neither an RRULE nor an RDATE beside a DTSTART.
func recurrenceOf(comp *ical.Component, loc *time.Location) (*recurrence, error) {
	split := splitDateLists(comp)
	hasRule := split.Props.Get(ical.PropRecurrenceRule) != nil
	hasDates := len(split.Props[ical.PropRecurrenceDates]) > 0 && split.Props.Get(ical.PropDateTimeStart) != nil
	if !hasRule && !hasDates {
		return nil, nil
	}
	if len(split.Props[ical.PropRecurrenceDates])+len(split.Props[ical.PropExceptionDates]) > maxRecurrenceIterations {
		return nil, errTooManyRecurrenceDates
	}

	rec := &recurrence{allDay: isDate(split.Props.Get(ical.PropDateTimeStart))}
	if hasRule {
		ruleOnly := *split
		ruleOnly.Props = maps.Clone(split.Props)
		ruleOnly.Props.Del(ical.PropRecurrenceDates)
		set, err := ruleOnly.RecurrenceSet(loc)
		if err != nil {
			return nil, err
		}
		rec.starts = set.Iterator
	}
	if !hasDates {
		return rec, nil
	}

	excluded, err := exceptionDates(split, loc)
	if err != nil {
		return nil, err
	}
	dates, err := recurrenceDates(split, loc)
	if err != nil {
		return nil, err
	}
	for _, d := range dates {
		if !excluded[d.start.Unix()] {
			rec.dates = append(rec.dates, d)
		}
	}

	if !hasRule {
		dtstart, err := split.Props.DateTime(ical.PropDateTimeStart, loc)
		if err != nil {
			return nil, err
		}
		rec.starts = func() func() (time.Time, bool) {
			pending := !excluded[dtstart.Unix()]
			return func() (time.Time, bool) {
				if !pending {
					return time.Time{}, false
				}
				pending = false
				return dtstart, true
			}
		}
	}
	return rec, nil
}

// iterator walks the set in ascending start order. A start shared by the rule
// and an RDATE is one occurrence, keeping the RDATE's period when it has one.
func (r *recurrence) iterator() func() (occurrence, bool) {
	starts := r.starts()
	next, more := starts()
	dates := r.dates
	return func() (occurrence, bool) {
		if len(dates) == 0 || (more && next.Before(dates[0].start)) {
			if !more {
				return occurrence{}, false
			}
			occ := occurrence{start: next, date: r.allDay}
			next, more = starts()
			return occ, true
		}

		occ := dates[0]
		dates = dates[1:]
		if more && next.Equal(occ.start) {
			next, more = starts()
		}
		return occ, true
	}
}

// recurrenceDates reads a split component's RDATE values, ascending and one
// per start. A period wins over a plain date at the same start, and the longer
// of two periods wins, since either way the time is taken.
func recurrenceDates(split *ical.Component, loc *time.Location) ([]occurrence, error) {
	props := split.Props[ical.PropRecurrenceDates]
	if len(props) > maxRecurrenceIterations {
		return nil, errTooManyRecurrenceDates
	}

	dates := make([]occurrence, 0, len(props))
	for i := range props {
		d, err := recurrenceDate(&props[i], loc)
		if err != nil {
			return nil, err
		}
		dates = append(dates, d)
	}
	slices.SortFunc(dates, func(a, b occurrence) int { return a.start.Compare(b.start) })

	collapsed := dates[:0]
	for _, d := range dates {
		last := len(collapsed) - 1
		if last >= 0 && collapsed[last].start.Equal(d.start) {
			collapsed[last] = preferred(collapsed[last], d)
			continue
		}
		collapsed = append(collapsed, d)
	}
	return collapsed, nil
}

// preferred keeps the later end. A plain date has no end of its own, so any
// period beats it here.
func preferred(a, b occurrence) occurrence {
	if b.end.After(a.end) {
		return b
	}
	return a
}

func isDate(prop *ical.Prop) bool {
	return prop != nil && prop.ValueType() == ical.ValueDate
}

// recurrenceDate reads one RDATE value by its own type, which RFC 5545
// §3.8.5.2 does not tie to DTSTART's: a DATE is midnight in loc. A period is
// recognised by its separator rather than by VALUE=PERIOD, because
// resolveObjectTimes rewrites a zoned period to UTC and marks it DATE-TIME.
func recurrenceDate(prop *ical.Prop, loc *time.Location) (occurrence, error) {
	if !strings.Contains(prop.Value, periodSeparator) {
		start, err := prop.DateTime(loc)
		if err != nil {
			return occurrence{}, fmt.Errorf("caldav: invalid RDATE %q: %w", prop.Value, err)
		}
		return occurrence{start: start, date: isDate(prop)}, nil
	}

	if tzid := prop.Params.Get(ical.ParamTimezoneID); tzid != "" {
		zone, err := time.LoadLocation(tzid)
		if err != nil {
			return occurrence{}, fmt.Errorf("caldav: invalid RDATE %q: %w", prop.Value, err)
		}
		loc = zone
	}
	start, end, err := period(prop.Value, loc)
	if err != nil {
		return occurrence{}, fmt.Errorf("caldav: invalid RDATE %q: %w", prop.Value, err)
	}
	// RFC 5545 §3.3.9: a period's start is before its end, and its duration
	// is positive.
	if !end.After(start) {
		return occurrence{}, fmt.Errorf("caldav: RDATE period %q does not end after it starts", prop.Value)
	}
	return occurrence{start: start, end: end, own: true}, nil
}

// exceptionDates reads a split component's EXDATE values, keyed by second as
// rrule-go compares them.
func exceptionDates(split *ical.Component, loc *time.Location) (map[int64]bool, error) {
	props := split.Props[ical.PropExceptionDates]
	excluded := make(map[int64]bool, len(props))
	for i := range props {
		t, err := props[i].DateTime(loc)
		if err != nil {
			return nil, fmt.Errorf("caldav: invalid EXDATE %q: %w", props[i].Value, err)
		}
		excluded[t.Unix()] = true
	}
	return excluded, nil
}

// periodIndex holds the RDATE periods of an object's masters, EXDATE applied,
// each read once however many overrides consult it, and all charged to one
// budget.
type periodIndex struct {
	byUID  map[string][]occurrence
	budget int
}

func newPeriodIndex() *periodIndex {
	return &periodIndex{byUID: make(map[string][]occurrence), budget: maxRecurrenceIterations}
}

// at returns the RDATE period of master that starts at start, if it lists one.
func (p *periodIndex) at(master *ical.Component, start time.Time) (occurrence, bool, error) {
	uid := propValue(master, ical.PropUID)
	dates, read := p.byUID[uid]
	if !read {
		var err error
		if dates, err = p.read(master); err != nil {
			return occurrence{}, false, err
		}
		p.byUID[uid] = dates
	}

	i, found := slices.BinarySearchFunc(dates, start, func(d occurrence, t time.Time) int { return d.start.Compare(t) })
	if !found || !dates[i].own {
		return occurrence{}, false, nil
	}
	return dates[i], true, nil
}

func (p *periodIndex) read(master *ical.Component) ([]occurrence, error) {
	split := splitDateLists(master)
	listed := len(split.Props[ical.PropRecurrenceDates]) + len(split.Props[ical.PropExceptionDates])
	if listed > p.budget {
		return nil, errTooManyRecurrenceDates
	}
	p.budget -= listed

	excluded, err := exceptionDates(split, time.UTC)
	if err != nil {
		return nil, err
	}
	dates, err := recurrenceDates(split, time.UTC)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(dates, func(d occurrence) bool { return excluded[d.start.Unix()] }), nil
}
