package caldav

import (
	"errors"
	"net/http"
	"time"

	"github.com/emersion/go-ical"
	"github.com/mniehe/davkit/internal"
)

// maxExpandedInstances bounds how many components one expansion may emit. A
// window and a recurrence frequency are both client-controlled, so a fixed-size
// request otherwise buys work proportional to their product.
const maxExpandedInstances = 10_000

// maxExpandedBytes bounds what those instances may weigh. The count alone does
// not: each one is a full clone of its master, so ten thousand of a
// multi-megabyte component is tens of gigabytes — built whole, in memory, before
// the response encoder sees any of it.
const maxExpandedBytes = 32 << 20

// expansionBudget is spent across a whole REPORT, so a request expanding many
// objects is bounded by its total rather than once per object.
type expansionBudget struct {
	instances int
	bytes     int
}

func newExpansionBudget() *expansionBudget {
	return &expansionBudget{instances: maxExpandedInstances, bytes: maxExpandedBytes}
}

func (b *expansionBudget) spend(comp *ical.Component) error {
	if b.instances <= 0 {
		return internal.HTTPErrorf(http.StatusForbidden, "caldav: expanding this request would yield more than %d instances", maxExpandedInstances)
	}
	size := componentSize(comp)
	if b.bytes < size {
		return internal.HTTPErrorf(http.StatusForbidden, "caldav: expanding this request would yield more than %d bytes", maxExpandedBytes)
	}
	b.instances--
	b.bytes -= size
	return nil
}

// componentSize approximates a component's serialised size — close enough to
// price an expansion that clones it many times over.
func componentSize(comp *ical.Component) int {
	n := len(comp.Name)
	for name, props := range comp.Props {
		for i := range props {
			n += len(name) + len(props[i].Value)
		}
	}
	for _, child := range comp.Children {
		n += componentSize(child)
	}
	return n
}

// expandCalendar rewrites cal as the recurrence instances intersecting the
// requested window, per RFC 4791 §9.6.5: each recurring component is replaced by
// one component per occurrence carrying RECURRENCE-ID, the instances drop the
// recurrence properties that generated them, VTIMEZONE components are removed,
// and the times they referenced are written as UTC.
//
// The server performs this rather than the backend, so every backend answers
// expand identically. A Backend must therefore return unexpanded data and leave
// CalendarCompRequest.Expand alone.
func expandCalendar(cal *ical.Calendar, req *calendarExpandRequest) (*ical.Calendar, error) {
	return expandCalendarWithin(cal, req, newExpansionBudget())
}

// expandCalendarWithin draws from a budget the caller owns, so one REPORT
// expanding many objects is bounded by its total rather than per object.
func expandCalendarWithin(cal *ical.Calendar, req *calendarExpandRequest, budget *expansionBudget) (*ical.Calendar, error) {
	// An object may carry its zones as embedded VTIMEZONEs that time.LoadLocation
	// cannot serve; resolving them to UTC up front lets the expansion below read
	// every time the same way. The result is UTC regardless, so normalising the
	// input changes nothing an unexpanded read would have returned.
	cal, resolveErr := resolveObjectTimes(cal, nil)
	if resolveErr != nil {
		return nil, resolveErr
	}

	root := cloneComponent(cal.Component)
	root.Children = nil
	out := &ical.Calendar{Component: root}

	emit := func(comp *ical.Component) error {
		if err := budget.spend(comp); err != nil {
			return err
		}
		out.Children = append(out.Children, comp)
		return nil
	}

	masters, overrides, plain, err := partitionForExpansion(cal)
	if err != nil {
		return nil, err
	}

	byKey := make(map[string]*recurrenceOverride, len(overrides))
	for i := range overrides {
		byKey[overrides[i].key] = &overrides[i]
	}

	consumed := make(map[string]bool, len(overrides))
	for _, master := range masters {
		if err := expandMaster(master, req, byKey, consumed, emit); err != nil {
			return nil, err
		}
	}

	// An override whose RECURRENCE-ID no longer matches an occurrence of the
	// rule still describes a real instance, so report it on its own.
	for _, ov := range overrides {
		if consumed[ov.key] {
			continue
		}
		ovStart, ovEnd, err := compInterval(ov.comp, time.UTC)
		if err != nil {
			return nil, internal.HTTPErrorf(http.StatusForbidden, "caldav: cannot expand recurrence: invalid override interval")
		}
		if !intervalOverlaps(req.Start, req.End, ovStart, ovEnd) {
			continue
		}
		if err := emit(instanceTimesToUTC(cloneComponent(ov.comp))); err != nil {
			return nil, err
		}
	}

	for _, comp := range plain {
		if comp.Name == ical.CompEvent {
			match, err := matchCompTimeRange(req.Start, req.End, comp)
			if err != nil {
				return nil, err
			}
			if !match {
				continue
			}
		}
		if err := emit(instanceTimesToUTC(cloneComponent(comp))); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// recurrenceMaster is a recurring component, paired with the set it generates
// so the rule and dates are only parsed once.
type recurrenceMaster struct {
	comp *ical.Component
	uid  string
	rec  *recurrence
}

type recurrenceIterator interface {
	Iterator() func() (time.Time, bool)
}

// recurrenceOverride is a component that redefines one instance of a recurring
// component, identified by UID plus RECURRENCE-ID.
type recurrenceOverride struct {
	comp         *ical.Component
	key          string
	recurrenceID time.Time
}

func partitionForExpansion(cal *ical.Calendar) (masters []recurrenceMaster, overrides []recurrenceOverride, plain []*ical.Component, err error) {
	for _, child := range cal.Children {
		if child.Name == ical.CompTimezone {
			continue
		}

		if prop := child.Props.Get(ical.PropRecurrenceID); prop != nil {
			recurrenceID, derr := prop.DateTime(time.UTC)
			if derr != nil {
				return nil, nil, nil, internal.HTTPErrorf(http.StatusForbidden, "caldav: cannot expand recurrence: invalid RECURRENCE-ID")
			}
			overrides = append(overrides, recurrenceOverride{
				comp:         child,
				key:          overrideKey(propValue(child, ical.PropUID), recurrenceID),
				recurrenceID: recurrenceID,
			})
			continue
		}

		rec, derr := recurrenceOf(child, time.UTC)
		if errors.Is(derr, errTooManyRecurrenceDates) {
			return nil, nil, nil, expansionOverflow()
		}
		if derr != nil {
			return nil, nil, nil, internal.HTTPErrorf(http.StatusForbidden, "caldav: cannot expand recurrence: invalid recurrence rule")
		}
		if rec == nil {
			plain = append(plain, child)
			continue
		}
		masters = append(masters, recurrenceMaster{
			comp: child,
			uid:  propValue(child, ical.PropUID),
			rec:  rec,
		})
	}
	return masters, overrides, plain, nil
}

func expandMaster(master recurrenceMaster, req *calendarExpandRequest, byKey map[string]*recurrenceOverride, consumed map[string]bool, emit func(*ical.Component) error) error {
	extent, err := extentOf(master.comp, time.UTC)
	if err != nil {
		return internal.HTTPErrorf(http.StatusForbidden, "caldav: cannot expand recurrence: invalid component interval")
	}
	next := master.rec.iterator()
	// Occurrences increase, so the walk ends either when the set runs out or
	// when their starts pass the window. Reaching the iteration cap instead means
	// the answer would be incomplete, which is worse than refusing. An occurrence
	// beginning before the window can still reach into it.
	for i := 0; i < maxRecurrenceIterations; i++ {
		occ, ok := next()
		if !ok {
			return nil
		}
		if occ.start.After(req.End) {
			return nil
		}

		// An override replaces the occurrence wherever it moved it, so it is
		// judged on its own time, and claimed even when the original time is
		// outside the window so the pass over unclaimed overrides skips it.
		key := overrideKey(master.uid, occ.start)
		if ov, found := byKey[key]; found {
			consumed[key] = true
			if err := emitOverride(ov, req, emit); err != nil {
				return err
			}
			continue
		}

		if !extent.overlaps(req.Start, req.End, occ) {
			continue
		}

		instance, err := recurrenceInstance(master.comp, occ)
		if err != nil {
			return err
		}
		if err := emit(instance); err != nil {
			return err
		}
	}
	return expansionOverflow()
}

func emitOverride(ov *recurrenceOverride, req *calendarExpandRequest, emit func(*ical.Component) error) error {
	overlaps, err := compOverlaps(req.Start, req.End, ov.comp)
	if err != nil {
		return internal.HTTPErrorf(http.StatusForbidden, "caldav: cannot expand recurrence: invalid override interval")
	}
	if !overlaps {
		return nil
	}
	return emit(instanceTimesToUTC(cloneComponent(ov.comp)))
}

func expansionOverflow() error {
	return internal.HTTPErrorf(http.StatusForbidden, "caldav: recurrence expansion exceeded %d iterations", maxRecurrenceIterations)
}

// recurrenceProps generate a set; an instance of it carries none of them.
var recurrenceProps = map[string]bool{
	ical.PropRecurrenceRule:  true,
	ical.PropRecurrenceDates: true,
	ical.PropExceptionDates:  true,
	"EXRULE":                 true,
}

// recurrenceInstance materialises one occurrence of a recurring component: the
// properties that generated it are dropped, RECURRENCE-ID identifies it, and
// its start moves to the occurrence. An event's DTEND and a to-do's DUE move
// with it, keeping the master's length, unless an RDATE period gives the
// occurrence its own end. A DATE series stays DATE.
func recurrenceInstance(master *ical.Component, occ occurrence) (*ical.Component, error) {
	instance := &ical.Component{Name: master.Name, Props: make(ical.Props, len(master.Props))}
	for name, props := range master.Props {
		if !recurrenceProps[name] {
			instance.Props[name] = cloneProps(props)
		}
	}
	for _, child := range master.Children {
		instance.Children = append(instance.Children, cloneComponent(child))
	}

	startProp := master.Props.Get(ical.PropDateTimeStart)
	allDay := isDate(startProp)
	setInstant(instance.Props, ical.PropRecurrenceID, occ.start, allDay)
	setInstant(instance.Props, ical.PropDateTimeStart, occ.start, allDay)

	endName, ends := instanceEnds[master.Name]
	if !ends {
		return instance, nil
	}
	if occ.own {
		instance.Props.Del(ical.PropDuration)
		instance.Props.SetDateTime(endName, occ.end.UTC())
		return instance, nil
	}

	endProp := master.Props.Get(endName)
	if endProp == nil || startProp == nil {
		return instance, nil
	}
	start, err := startProp.DateTime(time.UTC)
	if err != nil {
		return nil, internal.HTTPErrorf(http.StatusForbidden, "caldav: cannot expand recurrence: invalid DTSTART")
	}
	end, err := endProp.DateTime(time.UTC)
	if err != nil {
		return nil, internal.HTTPErrorf(http.StatusForbidden, "caldav: cannot expand recurrence: invalid %s", endName)
	}
	setInstant(instance.Props, endName, occ.start.Add(end.Sub(start)), isDate(endProp))
	return instance, nil
}

// instanceEnds names the property that ends an instance of each kind that has
// one.
var instanceEnds = map[string]string{
	ical.CompEvent: ical.PropDateTimeEnd,
	ical.CompToDo:  ical.PropDue,
}

func setInstant(props ical.Props, name string, t time.Time, date bool) {
	if date {
		props.SetDate(name, t.UTC())
		return
	}
	props.SetDateTime(name, t.UTC())
}

// instanceTimesToUTC rewrites the absolute times of an already-materialised
// component, so that a response carrying no VTIMEZONE stays self-describing. A
// DATE names a day in no zone and is left as it is.
func instanceTimesToUTC(comp *ical.Component) *ical.Component {
	for _, name := range []string{ical.PropDateTimeStart, ical.PropDateTimeEnd, ical.PropRecurrenceID} {
		prop := comp.Props.Get(name)
		if prop == nil || isDate(prop) {
			continue
		}
		t, err := prop.DateTime(time.UTC)
		if err != nil {
			// Leave a value the decoder could not read as it was rather than
			// replacing it with a zero time.
			continue
		}
		comp.Props.SetDateTime(name, t.UTC())
	}
	return comp
}

func overrideKey(uid string, recurrenceID time.Time) string {
	return uid + "\x00" + recurrenceID.UTC().Format(time.RFC3339)
}

func propValue(comp *ical.Component, name string) string {
	if prop := comp.Props.Get(name); prop != nil {
		return prop.Value
	}
	return ""
}

func cloneProps(props []ical.Prop) []ical.Prop {
	cloned := make([]ical.Prop, len(props))
	for i := range props {
		cloned[i] = ical.Prop{Name: props[i].Name, Value: props[i].Value}
		if props[i].Params != nil {
			cloned[i].Params = make(ical.Params, len(props[i].Params))
			for k, v := range props[i].Params {
				cloned[i].Params[k] = append([]string(nil), v...)
			}
		}
	}
	return cloned
}

func cloneComponent(comp *ical.Component) *ical.Component {
	out := &ical.Component{Name: comp.Name, Props: make(ical.Props, len(comp.Props))}
	for name, props := range comp.Props {
		out.Props[name] = cloneProps(props)
	}
	for _, child := range comp.Children {
		out.Children = append(out.Children, cloneComponent(child))
	}
	return out
}
