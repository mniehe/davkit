package caldav

import (
	"time"

	"github.com/emersion/go-ical"
)

// occurrenceExtent measures every occurrence of a recurring component by the
// RFC 4791 §9.9 rules for its kind, from the master's times read once, so a
// walk over many occurrences never materialises one to test it.
type occurrenceExtent struct {
	kind   string
	length time.Duration
	allDay bool
	todo   todoTimes
}

func extentOf(comp *ical.Component, loc *time.Location) (occurrenceExtent, error) {
	e := occurrenceExtent{kind: comp.Name, allDay: isDate(comp.Props.Get(ical.PropDateTimeStart))}
	switch comp.Name {
	case ical.CompEvent:
		start, end, err := compInterval(comp, loc)
		if err != nil {
			return occurrenceExtent{}, err
		}
		e.length = end.Sub(start)
	case ical.CompToDo:
		todo, err := todoTimesOf(comp, loc)
		if err != nil {
			return occurrenceExtent{}, err
		}
		e.todo = todo
	}
	return e, nil
}

// overlaps applies to occ the rule compOverlaps applies to a single component
// of the same kind. A period's own end stands in for the end the kind would
// otherwise derive.
func (e *occurrenceExtent) overlaps(start, end time.Time, occ occurrence) bool {
	switch e.kind {
	case ical.CompEvent:
		to := occ.start.Add(e.length)
		if occ.own {
			to = occ.end
		}
		return intervalOverlaps(start, end, occ.start, to)

	case ical.CompJournal:
		to := occ.start
		switch {
		case occ.own:
			to = occ.end
		case e.allDay:
			to = occ.start.AddDate(0, 0, 1)
		}
		return intervalOverlaps(start, end, occ.start, to)

	case ical.CompToDo:
		moved := e.todo.at(occ)
		return todoOverlaps(start, end, &moved)
	}
	return false
}

// at moves a master VTODO's times to occ: DTSTART to its start and DUE by the
// same amount, or to a period's own end.
func (t *todoTimes) at(occ occurrence) todoTimes {
	moved := *t
	if t.hasStart && t.hasDue {
		moved.due = t.due.Add(occ.start.Sub(t.start))
	}
	moved.start = occ.start
	if occ.own {
		moved.hasDue, moved.due, moved.hasDuration = true, occ.end, false
	}
	return moved
}
