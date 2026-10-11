package caldav

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/mniehe/davkit/internal"
)

// maxRecurrenceIterations bounds a time-range walk; FREQ=SECONDLY over a wide
// range would otherwise materialise billions of occurrences. Tripping it is an
// error rather than an answer: rrule-go has no seek (Between and After both wrap
// the same linear iterator), so a walk that ran out of budget has established
// nothing.
const maxRecurrenceIterations = 100_000

// filterObjects returns the filtered list of calendar objects matching the provided query.
// A nil query will return the full list of calendar objects.
func filterObjects(query *calendarQuery, cos []calendarObject) ([]calendarObject, error) {
	if query == nil {
		// FIXME: should we always return a copy of the provided slice?
		return cos, nil
	}

	var out []calendarObject
	for i := range cos {
		ok, err := matchObject(&query.CompFilter, &cos[i])
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		projected := projectObject(cos[i], &query.CompRequest)
		out = append(out, projected)
	}
	return out, nil
}

// matchObject reports whether the provided CalendarObject matches the query.
func matchObject(query *compFilter, co *calendarObject) (matched bool, err error) {
	if co.Data == nil || co.Data.Component == nil {
		return false, fmt.Errorf("caldav: calendar object %q has no parsed data", co.Path)
	}
	return match(query, co.Data.Component)
}

// match reports whether comp itself satisfies filter's positive conditions:
// the name, the time range, and the nested component and property filters.
//
// is-not-defined is a statement about a component's ABSENCE among its
// siblings, not about any one component, so it is never satisfied here — it is
// handled at the parent by matchCompFilter. A name mismatch is likewise a
// plain non-match, never a positive result.
func match(filter *compFilter, comp *ical.Component) (bool, error) {
	return matchAmong(filter, comp, nil)
}

// matchAmong is match for a component whose siblings may override some of its
// occurrences; overridden holds their overrideKey values.
func matchAmong(filter *compFilter, comp *ical.Component, overridden map[string]bool) (bool, error) {
	if comp.Name != filter.Name || filter.IsNotDefined {
		return false, nil
	}

	if hasTimeRange(filter) {
		ok, err := matchTimeRangeAmong(filter.Start, filter.End, comp, overridden)
		if err != nil || !ok {
			return false, err
		}
	}
	for i := range filter.Comps {
		ok, err := matchCompFilter(&filter.Comps[i], comp)
		if err != nil || !ok {
			return false, err
		}
	}
	for i := range filter.Props {
		ok, err := matchPropFilter(&filter.Props[i], comp)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// matchCompFilter reports whether comp has a child that satisfies filter
// (RFC 4791 §9.7.1). With is-not-defined it inverts: it matches exactly when
// no child carries the filter's name, so a component that is present can never
// satisfy an is-not-defined test.
func matchCompFilter(filter *compFilter, comp *ical.Component) (bool, error) {
	if filter.IsNotDefined {
		for _, child := range comp.Children {
			if child.Name == filter.Name {
				return false, nil
			}
		}
		return true, nil
	}

	var overridden map[string]bool
	if hasTimeRange(filter) {
		overridden = overrideKeys(comp.Children, filter.Start.Location())
	}
	for _, child := range comp.Children {
		ok, err := matchAmong(filter, child, overridden)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func hasTimeRange(filter *compFilter) bool {
	return !filter.Start.IsZero() || !filter.End.IsZero()
}

// overrideKeys names the instances that children carrying RECURRENCE-ID
// redefine (RFC 5545 §3.8.4.4). Such an instance is no longer where its series
// would put it, so a walk over the series must not find it there. A
// RECURRENCE-ID that does not read names no instance, so it takes none away.
func overrideKeys(children []*ical.Component, loc *time.Location) map[string]bool {
	var keys map[string]bool
	for _, child := range children {
		prop := child.Props.Get(ical.PropRecurrenceID)
		if prop == nil {
			continue
		}
		rid, err := prop.DateTime(loc)
		if err != nil {
			continue
		}
		if keys == nil {
			keys = make(map[string]bool)
		}
		keys[overrideKey(propValue(child, ical.PropUID), rid)] = true
	}
	return keys
}

func matchPropFilter(filter *propFilter, comp *ical.Component) (bool, error) {
	fields := comp.Props.Values(filter.Name)
	if len(fields) == 0 {
		return filter.IsNotDefined, nil
	}
	if filter.IsNotDefined {
		return false, nil
	}

	// A property may occur several times. One occurrence has to satisfy every
	// condition: gathering them from different occurrences would match an object
	// that no single property justifies.
	for i := range fields {
		match, err := matchPropField(filter, &fields[i])
		if err != nil {
			return false, err
		}
		if match {
			return true, nil
		}
	}
	return false, nil
}

func matchPropField(filter *propFilter, field *ical.Prop) (bool, error) {
	for _, paramFilter := range filter.ParamFilter {
		match, err := matchParamFilter(paramFilter, field)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}

	if !filter.Start.IsZero() || !filter.End.IsZero() {
		return matchPropTimeRange(filter.Start, filter.End, field)
	}
	if filter.TextMatch != nil {
		return matchTextMatch(*filter.TextMatch, field.Value)
	}
	// empty prop-filter, property exists
	return true, nil
}

// compInterval returns the span a component occupies. RFC 4791 §9.9 matches a
// VEVENT on its interval, not on DTSTART. A non-VEVENT occupies the instant at
// its start.
func compInterval(comp *ical.Component, loc *time.Location) (start, end time.Time, err error) {
	if comp.Name != ical.CompEvent {
		start, err = comp.Props.DateTime(ical.PropDateTimeStart, loc)
		return start, start, err
	}
	event := ical.Event{Component: comp}
	if start, err = event.DateTimeStart(loc); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if end, err = event.DateTimeEnd(loc); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if end.Before(start) {
		end = start
	}
	return start, end, nil
}

// intervalOverlaps reports whether [compStart, compEnd) overlaps [start, end);
// a zero bound is unbounded. RFC 4791 §9.9 gives a zero-length component its own
// rule, start <= DTSTART < end, so an instant on the lower bound is inside while
// an event merely ending there is not.
func intervalOverlaps(start, end, compStart, compEnd time.Time) bool {
	if !end.IsZero() && !compStart.Before(end) {
		return false
	}
	if start.IsZero() {
		return true
	}
	if compEnd.After(compStart) {
		return compEnd.After(start)
	}
	return !compStart.Before(start)
}

// dateTimeValue parses a raw iCalendar date-time through go-ical, so period
// values are read the same way property values are.
func dateTimeValue(value string, loc *time.Location) (time.Time, error) {
	prop := ical.NewProp(ical.PropDateTimeStart)
	prop.Value = value
	return prop.DateTime(loc)
}

// period reads one RFC 5545 §3.3.9 period: a start followed by either an end
// instant or a duration.
func period(value string, loc *time.Location) (start, end time.Time, err error) {
	from, to, ok := strings.Cut(value, "/")
	if !ok {
		return time.Time{}, time.Time{}, fmt.Errorf("caldav: malformed period %q", value)
	}
	if start, err = dateTimeValue(from, loc); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if to == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("caldav: period %q has no end", value)
	}
	if strings.ContainsAny(to[:1], "P+-") {
		prop := ical.NewProp(ical.PropDuration)
		prop.Value = to
		dur, derr := prop.Duration()
		if derr != nil {
			return time.Time{}, time.Time{}, derr
		}
		return start, start.Add(dur), nil
	}
	end, err = dateTimeValue(to, loc)
	return start, end, err
}

// freeBusyOverlaps applies the RFC 4791 §9.9 VFREEBUSY table. Its DTSTART/DTEND
// row is inclusive on DTEND, unlike every other component, so a period ending
// exactly at the window start still overlaps. Any DURATION is ignored, as the
// table says.
func freeBusyOverlaps(start, end time.Time, comp *ical.Component, loc *time.Location) (bool, error) {
	dtStart, dtEnd := comp.Props.Get(ical.PropDateTimeStart), comp.Props.Get(ical.PropDateTimeEnd)
	if dtStart != nil && dtEnd != nil {
		from, err := dtStart.DateTime(loc)
		if err != nil {
			return false, err
		}
		to, err := dtEnd.DateTime(loc)
		if err != nil {
			return false, err
		}
		return (end.IsZero() || end.After(from)) && (start.IsZero() || !start.After(to)), nil
	}

	for _, prop := range comp.Props.Values(ical.PropFreeBusy) {
		for _, value := range strings.Split(prop.Value, ",") {
			from, to, err := period(value, loc)
			if err != nil {
				return false, err
			}
			if (end.IsZero() || end.After(from)) && (start.IsZero() || to.After(start)) {
				return true, nil
			}
		}
	}
	return false, nil
}

// optDateTime reads a date-time property that may be absent.
func optDateTime(comp *ical.Component, name string, loc *time.Location) (value time.Time, ok bool, err error) {
	prop := comp.Props.Get(name)
	if prop == nil {
		return time.Time{}, false, nil
	}
	value, err = prop.DateTime(loc)
	return value, err == nil, err
}

// todoTimes are the VTODO properties the RFC 4791 §9.9 table reads. duration
// is only read where a row uses it, so an unreadable one elsewhere is ignored
// as before.
type todoTimes struct {
	start, due, completed, created             time.Time
	hasStart, hasDue, hasCompleted, hasCreated bool
	duration                                   time.Duration
	hasDuration                                bool
}

func todoTimesOf(comp *ical.Component, loc *time.Location) (todoTimes, error) {
	var t todoTimes
	var err error
	if t.start, t.hasStart, err = optDateTime(comp, ical.PropDateTimeStart, loc); err != nil {
		return todoTimes{}, err
	}
	if t.due, t.hasDue, err = optDateTime(comp, ical.PropDue, loc); err != nil {
		return todoTimes{}, err
	}
	if t.completed, t.hasCompleted, err = optDateTime(comp, ical.PropCompleted, loc); err != nil {
		return todoTimes{}, err
	}
	if t.created, t.hasCreated, err = optDateTime(comp, ical.PropCreated, loc); err != nil {
		return todoTimes{}, err
	}
	durProp := comp.Props.Get(ical.PropDuration)
	t.hasDuration = durProp != nil
	if t.hasStart && t.hasDuration && !t.hasDue {
		if t.duration, err = durProp.Duration(); err != nil {
			return todoTimes{}, err
		}
	}
	return t, nil
}

// todoOverlaps applies the RFC 4791 §9.9 VTODO table. Which row applies depends
// on which of DTSTART, DURATION, DUE, COMPLETED and CREATED the component
// carries, and the rows mix inclusive and exclusive bounds, so each is written
// out rather than folded into a shared interval test. A zero filter bound is
// unbounded and satisfies its side of every comparison.
func todoOverlaps(start, end time.Time, t *todoTimes) bool {
	startAtOrBefore := func(x time.Time) bool { return start.IsZero() || !start.After(x) }
	startBefore := func(x time.Time) bool { return start.IsZero() || start.Before(x) }
	endAfter := func(x time.Time) bool { return end.IsZero() || end.After(x) }
	endAtOrAfter := func(x time.Time) bool { return end.IsZero() || !end.Before(x) }

	switch {
	case t.hasStart && t.hasDuration && !t.hasDue:
		deadline := t.start.Add(t.duration)
		return startAtOrBefore(deadline) && (endAfter(t.start) || endAtOrAfter(deadline))

	case t.hasStart && !t.hasDuration && t.hasDue:
		return (startBefore(t.due) || startAtOrBefore(t.start)) && (endAfter(t.start) || endAtOrAfter(t.due))

	case t.hasStart && !t.hasDuration && !t.hasDue:
		return startAtOrBefore(t.start) && endAfter(t.start)

	case !t.hasStart && !t.hasDuration && t.hasDue:
		return startBefore(t.due) && endAtOrAfter(t.due)

	case !t.hasStart && !t.hasDuration && !t.hasDue && t.hasCompleted && t.hasCreated:
		return (startAtOrBefore(t.created) || startAtOrBefore(t.completed)) &&
			(endAtOrAfter(t.created) || endAtOrAfter(t.completed))

	case !t.hasStart && !t.hasDuration && !t.hasDue && t.hasCompleted && !t.hasCreated:
		return startAtOrBefore(t.completed) && endAtOrAfter(t.completed)

	case !t.hasStart && !t.hasDuration && !t.hasDue && !t.hasCompleted && t.hasCreated:
		return endAfter(t.created)
	}
	// The table's last row: a VTODO carrying none of these overlaps everything.
	return true
}

// compOverlaps applies the RFC 4791 §9.9 table for the component's own type. A
// type the table does not cover never overlaps, which is what the RFC means by
// leaving time-range undefined for it.
func compOverlaps(start, end time.Time, comp *ical.Component) (bool, error) {
	loc := start.Location()
	switch comp.Name {
	case ical.CompEvent:
		compStart, compEnd, err := compInterval(comp, loc)
		if err != nil {
			return false, err
		}
		return intervalOverlaps(start, end, compStart, compEnd), nil

	case ical.CompJournal:
		// A DATE-TIME DTSTART is an instant and a DATE one lasts a day, which is
		// the same split intervalOverlaps already makes.
		prop := comp.Props.Get(ical.PropDateTimeStart)
		if prop == nil {
			return false, nil
		}
		from, err := prop.DateTime(loc)
		if err != nil {
			return false, err
		}
		to := from
		if prop.ValueType() == ical.ValueDate {
			to = from.AddDate(0, 0, 1)
		}
		return intervalOverlaps(start, end, from, to), nil

	case ical.CompToDo:
		times, err := todoTimesOf(comp, loc)
		if err != nil {
			return false, err
		}
		return todoOverlaps(start, end, &times), nil

	case ical.CompFreeBusy:
		return freeBusyOverlaps(start, end, comp, loc)
	}
	return false, nil
}

func matchCompTimeRange(start, end time.Time, comp *ical.Component) (bool, error) {
	return matchTimeRangeAmong(start, end, comp, nil)
}

// matchTimeRangeAmong is matchCompTimeRange skipping the occurrences named in
// overridden, which siblings of comp redefine.
func matchTimeRangeAmong(start, end time.Time, comp *ical.Component, overridden map[string]bool) (bool, error) {
	// See https://datatracker.ietf.org/doc/html/rfc4791#section-9.9

	rec, err := recurrenceOf(comp, start.Location())
	if errors.Is(err, errTooManyRecurrenceDates) {
		return false, timeRangeOverflow()
	}
	if err != nil {
		return false, err
	}
	if rec == nil {
		return compOverlaps(start, end, comp)
	}

	// Each occurrence is measured by the rules for the component's kind, read
	// once from the master. Reading them here rather than above keeps a
	// component with no DTSTART out of compInterval, which cannot describe one.
	extent, err := extentOf(comp, start.Location())
	if err != nil {
		return false, err
	}

	// An occurrence beginning before the window can still reach into it, and a
	// VTODO's inclusive rows can reach one starting exactly at its end.
	uid := propValue(comp, ical.PropUID)
	next := rec.iterator()
	for n := 0; n < maxRecurrenceIterations; n++ {
		occ, ok := next()
		if !ok {
			return false, nil
		}
		if !end.IsZero() && occ.start.After(end) {
			// Occurrences only increase, so none remain in range.
			return false, nil
		}
		if !overridden[overrideKey(uid, occ.start)] && extent.overlaps(start, end, occ) {
			return true, nil
		}
		// Only a VTODO can match an occurrence starting on the end, and this
		// one has been tested; a later one cannot, so asking for it would only
		// spend the iteration cap.
		if occ.start.Equal(end) {
			return false, nil
		}
	}
	// Answering true here would return data the filter excluded: nothing has been
	// established, only that the budget ran out.
	return false, timeRangeOverflow()
}

func timeRangeOverflow() error {
	return internal.HTTPErrorf(http.StatusInsufficientStorage, "caldav: time-range filter exceeded %d recurrence iterations", maxRecurrenceIterations)
}

func matchPropTimeRange(start, end time.Time, field *ical.Prop) (bool, error) {
	// See https://datatracker.ietf.org/doc/html/rfc4791#section-9.9

	ptime, err := field.DateTime(start.Location())
	if err != nil {
		return false, err
	}
	return intervalOverlaps(start, end, ptime, ptime), nil
}

func matchParamFilter(filter paramFilter, field *ical.Prop) (bool, error) {
	values := field.Params.Values(filter.Name)
	// An empty first value still answers is-not-defined, which is what
	// ical.Params.Get reported before and what
	// TestParamFilterTreatsAnEmptyValueAsUndefined pins.
	if len(values) == 0 || values[0] == "" {
		return filter.IsNotDefined, nil
	}
	if filter.IsNotDefined {
		return false, nil
	}
	if filter.TextMatch == nil {
		return true, nil
	}
	// A parameter may carry several values; matching any one of them matches.
	for _, value := range values {
		ok, err := matchTextMatch(*filter.TextMatch, value)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// fold applies txt.Collation, defaulting to i;ascii-casemap (RFC 4791 §7.5.1)
// so an unqualified text-match is case-insensitive.
func fold(txt textMatch, s string) (string, error) {
	folded, err := internal.FoldForCollation(txt.Collation, internal.CollationASCIICasemap, s)
	if errors.Is(err, internal.ErrUnsupportedCollation) {
		return "", internal.NewPreconditionError(http.StatusForbidden, supportedCollationName)
	}
	return folded, err
}

func matchTextMatch(txt textMatch, value string) (bool, error) {
	needle, err := fold(txt, txt.Text)
	if err != nil {
		return false, err
	}
	haystack, err := fold(txt, value)
	if err != nil {
		return false, err
	}

	match := strings.Contains(haystack, needle)
	if txt.NegateCondition {
		match = !match
	}
	return match, nil
}
