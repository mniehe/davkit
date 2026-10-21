package caldav

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

const rangeThisAndFuture = "THISANDFUTURE"

// errMalformedRangedOverride is returned when an override whose RECURRENCE-ID
// carries RANGE=THISANDFUTURE has no start or length that reads. Ignoring the
// range instead would place every later instance where it no longer is.
var errMalformedRangedOverride = errors.New("caldav: a ranged override's start or length does not read")

// errInstantOutOfRange is returned when moving a time would leave the years
// an iCalendar DATE-TIME can write.
var errInstantOutOfRange = errors.New("caldav: a moved time falls outside the years iCalendar can write")

// earliestInstant and latestInstant bound the times a four-digit iCalendar
// year can name.
var (
	earliestInstant = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	latestInstant   = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)
)

// shiftInstant moves t by a signed number of seconds. A range's move can span
// millennia, which time.Duration cannot hold, and iCalendar times have no
// finer precision than the second.
func shiftInstant(t time.Time, seconds int64) (time.Time, error) {
	if t.Before(earliestInstant) || t.After(latestInstant) {
		return time.Time{}, errInstantOutOfRange
	}
	unix := t.Unix()
	if seconds > latestInstant.Unix()-unix || seconds < earliestInstant.Unix()-unix {
		return time.Time{}, errInstantOutOfRange
	}
	return time.Unix(unix+seconds, int64(t.Nanosecond())).In(t.Location()), nil
}

// secondsBetween is to minus from in whole seconds, which for two times
// shiftInstant accepts cannot overflow.
func secondsBetween(from, to time.Time) int64 {
	return to.Unix() - from.Unix()
}

// rangedOverride redefines every occurrence from recurrenceID on (RFC 5545
// §3.8.4.4): each moves by delta seconds and takes the override's extent.
type rangedOverride struct {
	comp         *ical.Component
	recurrenceID time.Time
	delta        int64
	extent       occurrenceExtent
	date         bool
	// earliest and latest bound the times an instance moves, so an
	// occurrence they cannot both follow has nothing to write.
	earliest, latest time.Time
}

// overrideIndex holds the overrides of one UID.
type overrideIndex struct {
	exact map[int64]*ical.Component
	// ranged ascends by RECURRENCE-ID.
	ranged []rangedOverride
}

// buildOverrideIndexes indexes by UID every child carrying a RECURRENCE-ID. A
// RECURRENCE-ID that does not read names no instance, so it overrides none.
func buildOverrideIndexes(children []*ical.Component, loc *time.Location) (map[string]*overrideIndex, error) {
	var indexes map[string]*overrideIndex
	for _, child := range children {
		prop := child.Props.Get(ical.PropRecurrenceID)
		if prop == nil {
			continue
		}
		rid, err := prop.DateTime(loc)
		if err != nil {
			continue
		}

		if indexes == nil {
			indexes = make(map[string]*overrideIndex)
		}
		uid := propValue(child, ical.PropUID)
		idx := indexes[uid]
		if idx == nil {
			idx = &overrideIndex{exact: make(map[int64]*ical.Component)}
			indexes[uid] = idx
		}
		idx.exact[rid.Unix()] = child

		if !isRanged(prop) {
			continue
		}
		ranged, err := readRanged(child, rid, loc)
		if err != nil {
			return nil, err
		}
		idx.ranged = append(idx.ranged, ranged)
	}

	for _, idx := range indexes {
		slices.SortStableFunc(idx.ranged, func(a, b rangedOverride) int { return a.recurrenceID.Compare(b.recurrenceID) })
	}
	return indexes, nil
}

func isRanged(rid *ical.Prop) bool {
	return strings.EqualFold(rid.Params.Get(ical.ParamRange), rangeThisAndFuture)
}

func readRanged(comp *ical.Component, rid time.Time, loc *time.Location) (rangedOverride, error) {
	startProp := comp.Props.Get(ical.PropDateTimeStart)
	if startProp == nil {
		return rangedOverride{}, fmt.Errorf("%w: no DTSTART", errMalformedRangedOverride)
	}
	start, err := startProp.DateTime(loc)
	if err != nil {
		return rangedOverride{}, fmt.Errorf("%w: %w", errMalformedRangedOverride, err)
	}
	extent, err := extentOf(comp, loc)
	if err != nil {
		return rangedOverride{}, fmt.Errorf("%w: %w", errMalformedRangedOverride, err)
	}
	delta := secondsBetween(rid, start)
	if _, err := shiftInstant(rid, delta); err != nil {
		return rangedOverride{}, fmt.Errorf("%w: %w", errMalformedRangedOverride, err)
	}
	earliest, latest := start, start
	for _, name := range []string{ical.PropDateTimeEnd, ical.PropDue} {
		// One that does not read is left as it is on the instance, so it
		// moves nowhere.
		t, ok, err := optDateTime(comp, name, loc)
		if err != nil || !ok {
			continue
		}
		earliest, latest = minTime(earliest, t), maxTime(latest, t)
	}
	return rangedOverride{
		comp:         comp,
		recurrenceID: rid,
		delta:        delta,
		extent:       extent,
		date:         isDate(startProp),
		earliest:     earliest,
		latest:       latest,
	}, nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// writable reports whether the instance r makes of an occurrence shift
// seconds from its anchor has every time inside the writable years.
func (r *rangedOverride) writable(shift int64) bool {
	if _, err := shiftInstant(r.earliest, shift); err != nil {
		return false
	}
	_, err := shiftInstant(r.latest, shift)
	return err == nil
}

// horizon is how far a walk over the series must go to find every original
// occurrence a range can move into a window ending at end. A range moving
// occurrences earlier reaches originals that far past end, but only up to the
// next range, which governs from there on. A range that governs nothing,
// because the next shares its anchor, or whose anchor lies beyond its reach,
// moves nothing into the window. A zero end is unbounded and stays so.
func (x *overrideIndex) horizon(end time.Time) time.Time {
	if x == nil || end.IsZero() {
		return end
	}
	horizon := end
	for i := range x.ranged {
		r := &x.ranged[i]
		if r.delta >= 0 {
			continue
		}
		hasNext := i+1 < len(x.ranged)
		if hasNext && !x.ranged[i+1].recurrenceID.After(r.recurrenceID) {
			continue
		}
		reach, err := shiftInstant(end, -r.delta)
		if err != nil {
			reach = latestInstant
		}
		if r.recurrenceID.After(reach) {
			continue
		}
		if hasNext && x.ranged[i+1].recurrenceID.Before(reach) {
			reach = x.ranged[i+1].recurrenceID
		}
		if reach.After(horizon) {
			horizon = reach
		}
	}
	return horizon
}

// instanceKind says which component an occurrence's instance comes from.
type instanceKind int

const (
	// fromMaster is an occurrence no override redefines.
	fromMaster instanceKind = iota
	// fromExact is an occurrence an override names by its exact
	// RECURRENCE-ID.
	fromExact
	// fromRange is an occurrence a ranged override governs.
	fromRange
	// beyondYears is an occurrence a range moves outside the years iCalendar
	// can write: there is no instance to write, so it yields none.
	beyondYears
)

// effectiveInstance is one occurrence as its overrides leave it: the
// component it takes its properties from, where the rule put it and where it
// is now. shift moves the template's own times to the instance's, in seconds.
type effectiveInstance struct {
	template  *ical.Component
	original  occurrence
	effective occurrence
	kind      instanceKind
	extent    *occurrenceExtent
	shift     int64
}

// seriesWalk resolves a master's occurrences, taken in ascending order, to the
// instances its overrides make of them.
type seriesWalk struct {
	master    *ical.Component
	extent    *occurrenceExtent
	overrides *overrideIndex
	// governing counts the ranged overrides at or before the last occurrence.
	governing int
}

func newSeriesWalk(master *ical.Component, extent *occurrenceExtent, overrides *overrideIndex) *seriesWalk {
	return &seriesWalk{master: master, extent: extent, overrides: overrides}
}

// resolve must be given occurrences in ascending order: the governing range
// is found by a cursor that only advances.
func (w *seriesWalk) resolve(occ occurrence) (effectiveInstance, error) {
	if w.overrides == nil {
		return effectiveInstance{template: w.master, original: occ, effective: occ, kind: fromMaster, extent: w.extent}, nil
	}

	ranged := w.overrides.ranged
	for w.governing < len(ranged) && !ranged[w.governing].recurrenceID.After(occ.start) {
		w.governing++
	}

	if exact, found := w.overrides.exact[occ.start.Unix()]; found {
		return effectiveInstance{template: exact, original: occ, kind: fromExact}, nil
	}
	if w.governing == 0 {
		return effectiveInstance{template: w.master, original: occ, effective: occ, kind: fromMaster, extent: w.extent}, nil
	}

	gov := &ranged[w.governing-1]
	shift := secondsBetween(gov.recurrenceID, occ.start)
	if !gov.writable(shift) {
		return effectiveInstance{template: gov.comp, original: occ, kind: beyondYears}, nil
	}
	start, err := shiftInstant(occ.start, gov.delta)
	if err != nil {
		return effectiveInstance{}, err
	}
	return effectiveInstance{
		template:  gov.comp,
		original:  occ,
		effective: occurrence{start: start, date: gov.date},
		kind:      fromRange,
		extent:    &gov.extent,
		shift:     shift,
	}, nil
}

func (w *seriesWalk) ranges() []rangedOverride {
	if w.overrides == nil {
		return nil
	}
	return w.overrides.ranged
}

// overlaps applies the RFC 4791 §9.9 rule for the instance's kind where it now
// is. An exact override is judged on its own times.
func (inst *effectiveInstance) overlaps(start, end time.Time) (bool, error) {
	if inst.kind == fromExact {
		return compOverlaps(start, end, inst.template)
	}
	return inst.extent.overlaps(start, end, inst.effective), nil
}

// materialise writes the instance as a component of its own. Its
// RECURRENCE-ID takes the value type of the master's DTSTART (RFC 5545
// §3.8.4.4), as recurrenceInstance gives one the master generates.
func (inst *effectiveInstance) materialise(master *ical.Component) (*ical.Component, error) {
	if inst.kind == fromMaster {
		return recurrenceInstance(master, inst.original)
	}
	date := isDate(master.Props.Get(ical.PropDateTimeStart)) && inst.original.date
	return overrideInstance(inst.template, inst.shift, inst.original.start, date)
}

// writerParams are the RECURRENCE-ID parameters overrideInstance decides
// itself rather than carrying over from the template.
var writerParams = map[string]bool{
	ical.ParamRange:      true,
	ical.ParamTimezoneID: true,
	ical.ParamValue:      true,
}

// overrideInstance materialises an override as the instance at original: the
// properties that generate a set are dropped, its start and end move by shift
// seconds in their own value types, and RECURRENCE-ID names original afresh,
// so a RANGE it was written with is not carried onto the instance. Its other
// RECURRENCE-ID parameters are kept.
func overrideInstance(template *ical.Component, shift int64, original time.Time, originalDate bool) (*ical.Component, error) {
	instance := &ical.Component{Name: template.Name, Props: make(ical.Props, len(template.Props))}
	for name, props := range template.Props {
		if !recurrenceProps[name] {
			instance.Props[name] = cloneProps(props)
		}
	}
	for _, child := range template.Children {
		instance.Children = append(instance.Children, cloneComponent(child))
	}

	for _, name := range []string{ical.PropDateTimeStart, ical.PropDateTimeEnd, ical.PropDue} {
		prop := instance.Props.Get(name)
		if prop == nil {
			continue
		}
		t, err := prop.DateTime(time.UTC)
		if err != nil {
			// Leave a value the decoder could not read as it was rather than
			// replacing it with a zero time.
			continue
		}
		moved, err := shiftInstant(t, shift)
		if err != nil {
			return nil, err
		}
		setInstant(instance.Props, name, moved, isDate(prop))
	}

	setInstant(instance.Props, ical.PropRecurrenceID, original, originalDate)
	if stored := template.Props.Get(ical.PropRecurrenceID); stored != nil {
		fresh := instance.Props.Get(ical.PropRecurrenceID)
		for name, values := range stored.Params {
			if !writerParams[name] {
				fresh.Params[name] = append([]string(nil), values...)
			}
		}
	}
	return instance, nil
}
