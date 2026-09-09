package caldav

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

const localDateTimeLayout = "20060102T150405"

// An RFC 5545 DATE-TIME carries a four-digit year, so these are the years a
// value can be written back out in.
const (
	minCalendarYear = 1
	maxCalendarYear = 9999
)

// dateTimeProps are the properties whose value is a DATE-TIME the engine reads
// as an instant. A floating one of these is resolved against a fallback zone;
// other properties carry no floating time worth resolving.
var dateTimeProps = map[string]bool{
	ical.PropDateTimeStart:   true,
	ical.PropDateTimeEnd:     true,
	ical.PropDue:             true,
	ical.PropRecurrenceID:    true,
	ical.PropExceptionDates:  true,
	ical.PropRecurrenceDates: true,
}

// resolverFromICS builds a resolver from an iCalendar object carrying a
// VTIMEZONE — a request's CALDAV:timezone element or a calendar's stored
// default. XML collapses CRLF to LF in element text, so the line endings are
// restored before the iCalendar decoder, which folds on CRLF, sees them.
func resolverFromICS(b []byte) (*tzResolver, error) {
	text := strings.ReplaceAll(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n", "\r\n")
	cal, err := ical.NewDecoder(strings.NewReader(text)).Decode()
	if err != nil {
		return nil, fmt.Errorf("caldav: timezone is not valid iCalendar: %w", err)
	}
	for _, child := range cal.Children {
		if child.Name == ical.CompTimezone {
			return newTZResolver(child)
		}
	}
	return nil, fmt.Errorf("caldav: timezone specification has no VTIMEZONE")
}

// resolveObjectTimes returns a copy of cal in which every DATE-TIME the engine
// will read is expressed in a zone Go can resolve. A value tagged with a TZID
// that names one of the object's own VTIMEZONE components — the case
// time.LoadLocation cannot serve — is rewritten to UTC and the TZID dropped; a
// floating value is rewritten against fallback when one is supplied. IANA-named
// and already-UTC values are left for go-ical to read as before.
//
// The stored object is never mutated: a plain calendar-query returns the bytes
// as they were written, TZID and all. Only the copy the matcher and the
// expander read is normalised.
func resolveObjectTimes(cal *ical.Calendar, fallback *tzResolver) (*ical.Calendar, error) {
	zones := make(map[string]*tzResolver)
	for _, child := range cal.Children {
		if child.Name != ical.CompTimezone {
			continue
		}
		// An unusable VTIMEZONE is left in place: go-ical will fail on its refs
		// with a message about that zone, which beats a silent wrong offset.
		if r, err := newTZResolver(child); err == nil {
			zones[r.id] = r
		}
	}
	if len(zones) == 0 && fallback == nil {
		return cal, nil
	}

	out := &ical.Calendar{Component: cloneComponent(cal.Component)}
	for _, child := range out.Children {
		if child.Name == ical.CompTimezone {
			continue
		}
		if err := resolveComponentTimes(child, zones, fallback); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func resolveComponentTimes(comp *ical.Component, zones map[string]*tzResolver, fallback *tzResolver) error {
	for name := range comp.Props {
		for i := range comp.Props[name] {
			prop := &comp.Props[name][i]
			tzid := prop.Params.Get(ical.ParamTimezoneID)
			switch {
			case tzid != "" && zones[tzid] != nil:
				if err := rewritePropToUTC(prop, zones[tzid]); err != nil {
					return err
				}
			case tzid == "" && fallback != nil && dateTimeProps[name]:
				if err := rewritePropToUTC(prop, fallback); err != nil {
					return err
				}
			}
		}
	}
	for _, child := range comp.Children {
		if err := resolveComponentTimes(child, zones, fallback); err != nil {
			return err
		}
	}
	return nil
}

// rewritePropToUTC converts a property's DATE-TIME value(s) to UTC using r and
// removes the TZID parameter. A DATE value, an already-UTC value, or anything
// that is not a local date-time is left untouched, so a mistyped property is
// passed through rather than turned into an error.
func rewritePropToUTC(prop *ical.Prop, r *tzResolver) error {
	if prop.ValueType() == ical.ValueDate {
		return nil
	}
	parts := strings.Split(prop.Value, ",")
	changed := false
	for i, part := range parts {
		converted, ok, err := localValueToUTC(part, r)
		if err != nil {
			return err
		}
		if ok {
			parts[i] = converted
			changed = true
		}
	}
	if !changed {
		return nil
	}
	prop.Value = strings.Join(parts, ",")
	prop.SetValueType(ical.ValueDateTime)
	delete(prop.Params, ical.ParamTimezoneID)
	return nil
}

// localValueToUTC converts one local DATE-TIME, or the start (and end) of one
// PERIOD, to UTC. It reports ok=false for a value it does not recognise as a
// local date-time so the caller leaves it as it was.
func localValueToUTC(value string, r *tzResolver) (utc string, converted bool, err error) {
	if start, rest, isPeriod := strings.Cut(value, "/"); isPeriod {
		startUTC, ok, err := localInstantToUTC(start, r)
		if err != nil || !ok {
			return "", false, err
		}
		// A period ends with either a duration, which is offset-free, or another
		// local date-time that needs the same conversion.
		if rest != "" && !strings.ContainsAny(rest[:1], "P+-") {
			endUTC, ok, err := localInstantToUTC(rest, r)
			if err != nil || !ok {
				return "", false, err
			}
			rest = endUTC
		}
		return startUTC + "/" + rest, true, nil
	}
	return localInstantToUTC(value, r)
}

func localInstantToUTC(value string, r *tzResolver) (utc string, converted bool, err error) {
	if len(value) != len(localDateTimeLayout) || strings.HasSuffix(value, "Z") {
		return "", false, nil
	}
	wall, err := time.Parse(localDateTimeLayout, value)
	if err != nil {
		return "", false, nil
	}
	offset, err := r.offsetAt(wall)
	if err != nil {
		return "", false, err
	}
	return wall.Add(-offset).Format(dateWithUTCTimeLayout), true, nil
}

// A tzResolver interprets an embedded VTIMEZONE: given a wall-clock reading in
// that zone it returns the UTC offset in effect, per RFC 5545 §3.6.5. Go's
// standard library cannot build a transition-aware *time.Location from anything
// but its own zoneinfo, so a calendar whose TZID is a private or vendor name —
// the mozilla.org zones, an Exchange export — has no Location to load and its
// times can only be resolved from the VTIMEZONE the object carries with it.
type tzResolver struct {
	id          string
	observances []observance
	// priorOffset is what the zone was on before any observance began: the
	// offset the earliest one moves away from. A reading older than every
	// onset resolves to this.
	priorOffset time.Duration
}

// observance is one STANDARD or DAYLIGHT sub-component: the offset it moves
// from and to, and the onsets at which it takes effect.
type observance struct {
	offsetFrom time.Duration
	offsetTo   time.Duration
	dtstart    time.Time // the first onset, a wall clock in offsetFrom
	set        recurrenceIterator
	rdates     []time.Time
}

// newTZResolver reads a VTIMEZONE component into a resolver. It rejects a
// definition with no usable observance rather than silently resolving every
// time to UTC.
func newTZResolver(vtz *ical.Component) (*tzResolver, error) {
	id, err := vtz.Props.Text(ical.PropTimezoneID)
	if err != nil || id == "" {
		return nil, fmt.Errorf("caldav: VTIMEZONE has no TZID")
	}

	r := &tzResolver{id: id}
	for _, sub := range vtz.Children {
		if sub.Name != ical.CompTimezoneStandard && sub.Name != ical.CompTimezoneDaylight {
			continue
		}
		obs, err := parseObservance(sub)
		if err != nil {
			return nil, fmt.Errorf("caldav: VTIMEZONE %q: %w", id, err)
		}
		r.observances = append(r.observances, obs)
	}
	if len(r.observances) == 0 {
		return nil, fmt.Errorf("caldav: VTIMEZONE %q has no STANDARD or DAYLIGHT sub-component", id)
	}

	earliest := &r.observances[0]
	for i := range r.observances {
		if r.observances[i].dtstart.Before(earliest.dtstart) {
			earliest = &r.observances[i]
		}
	}
	r.priorOffset = earliest.offsetFrom

	return r, nil
}

func parseObservance(sub *ical.Component) (observance, error) {
	from, err := utcOffset(sub, "TZOFFSETFROM")
	if err != nil {
		return observance{}, err
	}
	to, err := utcOffset(sub, "TZOFFSETTO")
	if err != nil {
		return observance{}, err
	}

	dtstart, err := sub.Props.DateTime(ical.PropDateTimeStart, time.UTC)
	if err != nil {
		return observance{}, fmt.Errorf("%s has no usable DTSTART: %w", sub.Name, err)
	}

	// RecurrenceSet folds RRULE, RDATE and DTSTART into one iterator, reading
	// the onsets as the wall clocks they are written in. A sub-component with no
	// RRULE returns nil, leaving the single DTSTART plus any RDATEs.
	onsets := shiftUntilToOnsetFrame(splitDateLists(sub), from)
	set, err := onsets.RecurrenceSet(time.UTC)
	if err != nil {
		return observance{}, fmt.Errorf("%s has an invalid recurrence: %w", sub.Name, err)
	}

	// A nil *rrule.Set must not be stored in the interface field: a typed nil
	// reads as non-nil through an interface, which would send onsetsThrough down
	// the wrong branch.
	obs := observance{offsetFrom: from, offsetTo: to, dtstart: dtstart}
	if set != nil {
		obs.set = set
		return obs, nil
	}
	for _, rd := range onsets.Props[ical.PropRecurrenceDates] {
		t, err := rd.DateTime(time.UTC)
		if err != nil {
			return observance{}, fmt.Errorf("%s has an invalid RDATE: %w", sub.Name, err)
		}
		obs.rdates = append(obs.rdates, t)
	}
	return obs, nil
}

// shiftUntilToOnsetFrame moves a UTC UNTIL into the frame its own onsets are
// written in.
//
// An observance's onsets are wall clocks in the offset being left, but §3.6.5
// requires UNTIL to name the last of them as a UTC instant. The recurrence
// engine compares the two in a single frame, so a bound left as written sits
// offsetFrom away from the onset it is meant to include: east of Greenwich it
// lands before that onset and drops it, taking the whole era's final year with
// it, while west of Greenwich it lands after and happens to be harmless. Every
// conforming producer emits this shape, so the correction belongs here rather
// than in whatever wrote the calendar.
func shiftUntilToOnsetFrame(comp *ical.Component, offsetFrom time.Duration) *ical.Component {
	rule := comp.Props.Get(ical.PropRecurrenceRule)
	if rule == nil {
		return comp
	}

	parts := strings.Split(rule.Value, ";")
	changed := false
	for i, part := range parts {
		value, ok := strings.CutPrefix(part, "UNTIL=")
		if !ok {
			continue
		}

		instant, err := time.ParseInLocation(localDateTimeLayout, strings.TrimSuffix(value, "Z"), time.UTC)
		if err != nil {
			continue
		}

		// Take whichever of the two readings is later. For a conforming UTC
		// bound east of Greenwich that is the shifted one, which is the whole
		// point. West of Greenwich the bound as written already sits at or
		// after the onset it names, so shifting would only risk moving it
		// backwards off that onset. The same choice repairs a producer that
		// wrote the onset's local clock with a stray Z, which the shift alone
		// would have made worse. Onsets are a year apart, so the later reading
		// can never reach the following one.
		bound := instant
		if shifted := instant.Add(offsetFrom); shifted.After(bound) {
			bound = shifted
		}

		// The conventional "forever" bound is 99991231T235959Z, and nudging
		// that forward yields a five-digit year no recurrence parser accepts,
		// turning a calendar that read perfectly well into one that cannot be
		// read at all.
		if bound.Year() > maxCalendarYear || bound.Year() < minCalendarYear {
			continue
		}

		// Written in the frame the onsets use, which is what the recurrence
		// engine compares it against.
		parts[i] = "UNTIL=" + bound.Format(localDateTimeLayout)
		changed = true
	}
	if !changed {
		return comp
	}

	shifted := *comp
	shifted.Props = maps.Clone(comp.Props)
	rewritten := *rule
	rewritten.Value = strings.Join(parts, ";")
	shifted.Props[ical.PropRecurrenceRule] = []ical.Prop{rewritten}

	return &shifted
}

// utcOffset reads an RFC 5545 UTC-offset property (±HHMM or ±HHMMSS).
func utcOffset(comp *ical.Component, name string) (time.Duration, error) {
	prop := comp.Props.Get(name)
	if prop == nil {
		return 0, fmt.Errorf("%s is missing %s", comp.Name, name)
	}
	return parseUTCOffset(prop.Value)
}

func parseUTCOffset(s string) (time.Duration, error) {
	if len(s) != 5 && len(s) != 7 {
		return 0, fmt.Errorf("caldav: malformed UTC offset %q", s)
	}
	sign := time.Duration(1)
	switch s[0] {
	case '+':
	case '-':
		sign = -1
	default:
		return 0, fmt.Errorf("caldav: UTC offset %q has no sign", s)
	}

	hours, err := strconv.Atoi(s[1:3])
	mins, err2 := strconv.Atoi(s[3:5])
	secs, err3 := 0, error(nil)
	if len(s) == 7 {
		secs, err3 = strconv.Atoi(s[5:7])
	}
	if err != nil || err2 != nil || err3 != nil {
		return 0, fmt.Errorf("caldav: malformed UTC offset %q", s)
	}
	return sign * (time.Duration(hours)*time.Hour + time.Duration(mins)*time.Minute + time.Duration(secs)*time.Second), nil
}

// transition is one onset: the UTC instant a new offset takes effect, plus the
// offsets on either side of it.
type transition struct {
	utc        time.Time
	offsetFrom time.Duration
	offsetTo   time.Duration
}

// offsetAt returns the UTC offset the zone applies to a wall-clock reading.
// wall is a naive local time — the value of a DATE-TIME property tagged with
// this zone's TZID, with no offset of its own.
//
// The onsets of a VTIMEZONE are themselves written in local time, so the
// applicable observance is the one whose onset is the latest at or before the
// reading. A daylight gap or overlap leaves an hour that is ambiguous or
// unreal; the walk resolves it to the later offset, which is what a client that
// wrote the time expects back.
func (r *tzResolver) offsetAt(wall time.Time) (time.Duration, error) {
	trans, err := r.transitionsThrough(wall.Year() + 1)
	if err != nil {
		return 0, err
	}
	// No onset at or before this reading means the zone had not started
	// changing yet, which is an answer rather than a failure: every rule in
	// the definition begins later than the moment being asked about.
	if len(trans) == 0 {
		return r.priorOffset, nil
	}

	for i := len(trans) - 1; i >= 0; i-- {
		if !wall.Add(-trans[i].offsetTo).Before(trans[i].utc) {
			return trans[i].offsetTo, nil
		}
	}
	// Before the first onset the zone is in the earliest observance's prior
	// offset. That has to come from the definition as a whole, not from the
	// transitions materialised for this reading: a reading older than every
	// rule leaves only the observances that carry no rule, so reading it off
	// those would answer with whichever of them happened to be listed.
	return r.priorOffset, nil
}

// transitionsThrough materialises every onset up to and including limitYear,
// sorted by the UTC instant it takes effect.
func (r *tzResolver) transitionsThrough(limitYear int) ([]transition, error) {
	var trans []transition
	for i := range r.observances {
		obs := &r.observances[i]
		onsets, err := obs.onsetsThrough(limitYear)
		if err != nil {
			return nil, err
		}
		for _, onset := range onsets {
			trans = append(trans, transition{
				utc:        onset.Add(-obs.offsetFrom),
				offsetFrom: obs.offsetFrom,
				offsetTo:   obs.offsetTo,
			})
		}
	}
	// Ascending by UTC instant. Each observance's own list is ascending but the
	// lists are unordered relative to each other, so an insertion pass here is
	// quadratic on a definition whose later observances start earlier — which
	// a hostile calendar can arrange in a few hundred bytes.
	slices.SortStableFunc(trans, func(a, b transition) int {
		return a.utc.Compare(b.utc)
	})

	return trans, nil
}

// onsetsThrough returns this observance's onset wall clocks up to limitYear.
func (obs *observance) onsetsThrough(limitYear int) ([]time.Time, error) {
	if obs.set == nil {
		onsets := append([]time.Time{obs.dtstart}, obs.rdates...)
		return onsets, nil
	}

	var onsets []time.Time
	iter := obs.set.Iterator()
	for n := 0; n < maxRecurrenceIterations; n++ {
		onset, ok := iter()
		if !ok {
			return onsets, nil
		}
		if onset.Year() > limitYear {
			return onsets, nil
		}
		onsets = append(onsets, onset)
	}
	return nil, fmt.Errorf("caldav: VTIMEZONE onset rule exceeded %d iterations", maxRecurrenceIterations)
}
