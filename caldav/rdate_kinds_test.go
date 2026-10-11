package caldav

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/mniehe/davkit/internal"
)

// componentICS wraps component bodies of one kind in a VCALENDAR.
func componentICS(name string, bodies ...string) string {
	ics := "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//EN\n"
	for _, body := range bodies {
		ics += "BEGIN:" + name + "\nDTSTAMP:20260101T000000Z\n" + body + "END:" + name + "\n"
	}
	return ics + "END:VCALENDAR\n"
}

func matchesKind(t *testing.T, cal *ical.Calendar, name string, start, end time.Time) bool {
	t.Helper()
	ok, err := match(vevent(name, start, end), cal.Component)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	return ok
}

func expandOne(t *testing.T, cal *ical.Calendar, name string, start, end time.Time) *ical.Component {
	t.Helper()
	out, err := expandCalendar(cal, &calendarExpandRequest{Start: start, End: end})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	comps := componentsNamed(out, name)
	if len(comps) != 1 {
		t.Fatalf("instances = %d, want 1", len(comps))
	}
	return comps[0]
}

func assertProp(t *testing.T, comp *ical.Component, name string, wantType ical.ValueType, wantValue string) {
	t.Helper()
	prop := comp.Props.Get(name)
	if prop == nil {
		t.Errorf("%s missing, want %s", name, wantValue)
		return
	}
	if prop.ValueType() != wantType || prop.Value != wantValue {
		t.Errorf("%s = %s %q, want %s %q", name, prop.ValueType(), prop.Value, wantType, wantValue)
	}
}

// RFC 4791 §9.9 measures a VTODO by DTSTART and DUE, not by DTSTART alone, and
// every occurrence is measured the same way.
func TestRecurringTodoIsMeasuredByItsDue(t *testing.T) {
	for name, recurrence := range map[string]string{
		"RDATE": "RDATE:20260802T090000Z\n",
		"RRULE": "RRULE:FREQ=DAILY;COUNT=2\n",
	} {
		t.Run(name, func(t *testing.T) {
			cal := parseICS(t, componentICS(ical.CompToDo, "UID:todo\nDTSTART:20260801T090000Z\nDUE:20260801T170000Z\n"+recurrence))

			for _, day := range []int{1, 2} {
				if !matchesKind(t, cal, ical.CompToDo, at(2026, time.August, day, 12, 0), at(2026, time.August, day, 13, 0)) {
					t.Errorf("12:00-13:00 on %d August, inside DTSTART..DUE, did not match", day)
				}
			}
			if matchesKind(t, cal, ical.CompToDo, at(2026, time.August, 2, 18, 0), at(2026, time.August, 2, 19, 0)) {
				t.Error("a window after the second occurrence's DUE matched")
			}
		})
	}
}

func TestExpandedTodoMovesItsDue(t *testing.T) {
	cal := parseICS(t, componentICS(ical.CompToDo, "UID:todo\nDTSTART:20260801T090000Z\nDUE:20260801T170000Z\nRDATE:20260808T090000Z\n"))

	todo := expandOne(t, cal, ical.CompToDo, at(2026, time.August, 8, 0, 0), at(2026, time.August, 9, 0, 0))

	assertProp(t, todo, ical.PropDateTimeStart, ical.ValueDateTime, "20260808T090000Z")
	assertProp(t, todo, ical.PropDue, ical.ValueDateTime, "20260808T170000Z")
}

// A period states the occurrence's own end, which for a VTODO is its DUE.
func TestTodoPeriodIsItsOwnDue(t *testing.T) {
	cal := parseICS(t, componentICS(ical.CompToDo, "UID:todo\nDTSTART:20260801T090000Z\nDUE:20260801T170000Z\nRDATE;VALUE=PERIOD:20260815T090000Z/PT2H\n"))

	if !matchesKind(t, cal, ical.CompToDo, at(2026, time.August, 15, 10, 30), at(2026, time.August, 15, 10, 45)) {
		t.Error("a window inside the period did not match")
	}
	if matchesKind(t, cal, ical.CompToDo, at(2026, time.August, 15, 12, 0), at(2026, time.August, 15, 13, 0)) {
		t.Error("a window past the period's end matched on the master's DUE")
	}
	todo := expandOne(t, cal, ical.CompToDo, at(2026, time.August, 15, 0, 0), at(2026, time.August, 16, 0, 0))
	assertProp(t, todo, ical.PropDue, ical.ValueDateTime, "20260815T110000Z")
}

// RFC 4791 §9.9: a VJOURNAL with a DATE DTSTART lasts that whole day.
func TestRecurringDateJournalSpansEachDay(t *testing.T) {
	cal := parseICS(t, componentICS(ical.CompJournal, "UID:journal\nDTSTART;VALUE=DATE:20260801\nRDATE;VALUE=DATE:20260810\n"))

	for _, day := range []int{1, 10} {
		if !matchesKind(t, cal, ical.CompJournal, at(2026, time.August, day, 12, 0), at(2026, time.August, day, 13, 0)) {
			t.Errorf("midday on %d August did not match the day-long entry", day)
		}
	}
	if matchesKind(t, cal, ical.CompJournal, at(2026, time.August, 5, 12, 0), at(2026, time.August, 5, 13, 0)) {
		t.Error("a day without an occurrence matched")
	}
}

// RFC 5545 §3.8.5.3: the instances of a DATE series are DATE too, and one
// stating no end lasts its day.
func TestAllDayInstancesStayDates(t *testing.T) {
	t.Run("RDATE without an end", func(t *testing.T) {
		cal := parseICS(t, eventICS("UID:allday\nDTSTART;VALUE=DATE:20260801\nRDATE;VALUE=DATE:20260808\n"))

		ev := expandOne(t, cal, ical.CompEvent, at(2026, time.August, 8, 0, 0), at(2026, time.August, 9, 0, 0))

		assertProp(t, ev, ical.PropDateTimeStart, ical.ValueDate, "20260808")
		assertProp(t, ev, ical.PropRecurrenceID, ical.ValueDate, "20260808")
	})
	t.Run("RRULE with an end", func(t *testing.T) {
		cal := parseICS(t, eventICS("UID:allday\nDTSTART;VALUE=DATE:20260801\nDTEND;VALUE=DATE:20260803\nRRULE:FREQ=WEEKLY;COUNT=2\n"))

		ev := expandOne(t, cal, ical.CompEvent, at(2026, time.August, 8, 0, 0), at(2026, time.August, 9, 0, 0))

		assertProp(t, ev, ical.PropDateTimeStart, ical.ValueDate, "20260808")
		assertProp(t, ev, ical.PropDateTimeEnd, ical.ValueDate, "20260810")
		assertProp(t, ev, ical.PropRecurrenceID, ical.ValueDate, "20260808")
	})
	t.Run("not recurring", func(t *testing.T) {
		cal := parseICS(t, eventICS("UID:allday\nDTSTART;VALUE=DATE:20260808\n"))

		ev := expandOne(t, cal, ical.CompEvent, at(2026, time.August, 8, 0, 0), at(2026, time.August, 9, 0, 0))

		assertProp(t, ev, ical.PropDateTimeStart, ical.ValueDate, "20260808")
	})
}

// RFC 5545 §3.8.5.2 lets each RDATE be a DATE, a DATE-TIME or a period,
// whatever DTSTART is, so each is read by its own type: a DATE as midnight, and
// the instance it yields carries that type on DTSTART and RECURRENCE-ID while
// keeping the series' length.
func TestMixedValueTypesAreReadEachByItsOwn(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		inside      span
		outside     span
		wantType    ical.ValueType
		wantStart   string
		wantEndType ical.ValueType
		wantEnd     string
	}{
		{
			name:        "DATE series, DATE-TIME RDATE",
			body:        "DTSTART;VALUE=DATE:20260801\nDTEND;VALUE=DATE:20260802\nRDATE:20260808T090000Z\n",
			inside:      span{at(2026, time.August, 9, 8, 0), at(2026, time.August, 9, 8, 30)},
			outside:     span{at(2026, time.August, 8, 8, 0), at(2026, time.August, 8, 8, 30)},
			wantType:    ical.ValueDateTime,
			wantStart:   "20260808T090000Z",
			wantEndType: ical.ValueDateTime,
			wantEnd:     "20260809T090000Z",
		},
		{
			name:        "DATE series without an end, DATE-TIME RDATE",
			body:        "DTSTART;VALUE=DATE:20260801\nRDATE:20260808T090000Z\n",
			inside:      span{at(2026, time.August, 9, 8, 0), at(2026, time.August, 9, 8, 30)},
			outside:     span{at(2026, time.August, 8, 8, 0), at(2026, time.August, 8, 8, 30)},
			wantType:    ical.ValueDateTime,
			wantStart:   "20260808T090000Z",
			wantEndType: ical.ValueDateTime,
			wantEnd:     "20260809T090000Z",
		},
		{
			name:        "DATE series, PERIOD RDATE",
			body:        "DTSTART;VALUE=DATE:20260801\nDTEND;VALUE=DATE:20260802\nRDATE;VALUE=PERIOD:20260808T090000Z/PT1H\n",
			inside:      span{at(2026, time.August, 8, 9, 15), at(2026, time.August, 8, 9, 30)},
			outside:     span{at(2026, time.August, 8, 11, 0), at(2026, time.August, 8, 12, 0)},
			wantType:    ical.ValueDateTime,
			wantStart:   "20260808T090000Z",
			wantEndType: ical.ValueDateTime,
			wantEnd:     "20260808T100000Z",
		},
		{
			name:        "DATE-TIME series, DATE RDATE",
			body:        "DTSTART:20260801T090000Z\nDTEND:20260801T100000Z\nRDATE;VALUE=DATE:20260808\n",
			inside:      span{at(2026, time.August, 8, 0, 15), at(2026, time.August, 8, 0, 30)},
			outside:     span{at(2026, time.August, 8, 9, 15), at(2026, time.August, 8, 9, 30)},
			wantType:    ical.ValueDate,
			wantStart:   "20260808",
			wantEndType: ical.ValueDateTime,
			wantEnd:     "20260808T010000Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cal := parseICS(t, eventICS("UID:mixed\n"+tc.body))

			if !matchesWindow(t, cal, tc.inside.start, tc.inside.end) {
				t.Error("a window inside the extra date's instance did not match")
			}
			if matchesWindow(t, cal, tc.outside.start, tc.outside.end) {
				t.Error("a window outside the extra date's instance matched")
			}
			ev := expandOne(t, cal, ical.CompEvent, at(2026, time.August, 8, 0, 0), at(2026, time.August, 8, 12, 0))
			assertProp(t, ev, ical.PropDateTimeStart, tc.wantType, tc.wantStart)
			assertProp(t, ev, ical.PropRecurrenceID, tc.wantType, tc.wantStart)
			assertProp(t, ev, ical.PropDateTimeEnd, tc.wantEndType, tc.wantEnd)
		})
	}
}

// limitRecurrenceCalendar reads a master's dates once for all its overrides,
// so the work an override adds does not grow with the master's RDATE list.
func TestLimitRecurrenceSetReadsAMastersDatesOnce(t *testing.T) {
	window := &calendarTimeWindow{Start: at(2030, time.January, 1, 0, 0), End: at(2030, time.January, 2, 0, 0)}
	perOverride := func(dates int) float64 {
		few := limitFixture(t, dates, 1)
		many := limitFixture(t, dates, 101)
		run := func(cal *ical.Calendar) float64 {
			return testing.AllocsPerRun(2, func() {
				if _, err := limitRecurrenceCalendar(cal, window); err != nil {
					t.Fatal(err)
				}
			})
		}
		return (run(many) - run(few)) / 100
	}

	small, large := perOverride(10), perOverride(5_000)
	if large > small+10 {
		t.Errorf("allocations per override: %.0f with 5000 RDATEs against %.0f with 10; each override re-reads the list", large, small)
	}
}

func limitFixture(t *testing.T, dates, overrides int) *ical.Calendar {
	t.Helper()
	values := make([]string, dates)
	for i := range values {
		values[i] = at(2026, time.January, 1, 9, 0).Add(time.Duration(i) * time.Hour).Format(dateWithUTCTimeLayout)
	}
	bodies := make([]string, 0, 1+overrides)
	bodies = append(bodies, "UID:lim\nDTSTART:20260101T090000Z\nDTEND:20260101T100000Z\nRDATE;VALUE=PERIOD:"+
		strings.ReplaceAll(strings.Join(values, ","), "Z,", "Z/PT1H,")+"/PT1H\n")
	for i := range overrides {
		bodies = append(bodies, fmt.Sprintf("UID:lim\nRECURRENCE-ID:%s\nDTSTART:20290101T090000Z\nDTEND:20290101T100000Z\n", values[i%dates]))
	}
	return parseICS(t, eventICS(bodies...))
}

// A DATE-TIME VJOURNAL is an instant, but a period gives an occurrence its own
// end.
func TestJournalPeriodKeepsItsOwnEnd(t *testing.T) {
	cal := parseICS(t, componentICS(ical.CompJournal, "UID:journal\nDTSTART:20260801T090000Z\nRDATE;VALUE=PERIOD:20260810T090000Z/PT3H\n"))

	if !matchesKind(t, cal, ical.CompJournal, at(2026, time.August, 10, 11, 0), at(2026, time.August, 10, 11, 30)) {
		t.Error("a window inside the period did not match")
	}
}

// The period replaces the master's DURATION, so the occurrence is read by the
// DTSTART and DUE row rather than by none.
func TestTodoPeriodReplacesTheMastersDuration(t *testing.T) {
	cal := parseICS(t, componentICS(ical.CompToDo, "UID:todo\nDTSTART:20260801T090000Z\nDURATION:PT1H\nRDATE;VALUE=PERIOD:20260815T090000Z/PT2H\n"))

	if matchesKind(t, cal, ical.CompToDo, at(2026, time.August, 15, 12, 0), at(2026, time.August, 15, 13, 0)) {
		t.Error("a window after the period matched")
	}
}

// RFC 4791 §9.9 gives a VTODO whose DTSTART equals its DUE an inclusive end,
// so an occurrence starting exactly at the window's end is inside it.
func TestTodoOccurrenceAtTheWindowsEndIsFound(t *testing.T) {
	cal := parseICS(t, componentICS(ical.CompToDo, "UID:todo\nDTSTART:20260801T090000Z\nDUE:20260801T090000Z\nRDATE:20260810T120000Z\n"))
	from, to := at(2026, time.August, 10, 11, 0), at(2026, time.August, 10, 12, 0)

	if !matchesKind(t, cal, ical.CompToDo, from, to) {
		t.Error("an occurrence due exactly at the window's end did not match")
	}
	todo := expandOne(t, cal, ical.CompToDo, from, to)
	assertProp(t, todo, ical.PropDateTimeStart, ical.ValueDateTime, "20260810T120000Z")
}

// Every master in one object draws on the same budget, so splitting a huge
// list across UIDs does not escape it.
func TestLimitRecurrenceSetChargesEveryMasterToOneBudget(t *testing.T) {
	half := maxRecurrenceIterations/2 + 1
	values := make([]string, half)
	for i := range values {
		values[i] = at(2026, time.January, 1, 9, 0).Add(time.Duration(i) * time.Minute).Format(dateWithUTCTimeLayout)
	}
	list := strings.Join(values, ",")
	bodies := make([]string, 0, 4)
	for _, uid := range []string{"a", "b"} {
		bodies = append(bodies,
			"UID:"+uid+"\nDTSTART:20260101T090000Z\nDTEND:20260101T100000Z\nRDATE:"+list+"\n",
			"UID:"+uid+"\nRECURRENCE-ID:20260101T090000Z\nDTSTART:20290101T090000Z\nDTEND:20290101T100000Z\n")
	}
	cal := parseICS(t, eventICS(bodies...))

	_, err := limitRecurrenceCalendar(cal, &calendarTimeWindow{Start: at(2030, time.January, 1, 0, 0), End: at(2030, time.January, 2, 0, 0)})
	var httpErr *internal.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != http.StatusForbidden || !strings.Contains(err.Error(), "iterations") {
		t.Errorf("err = %v, want 403 naming the iteration cap", err)
	}
}

// RFC 4791 §9.6.6 judges an override on its original time, which for a DATE
// VJOURNAL is the whole day its RECURRENCE-ID names.
func TestLimitRecurrenceSetJudgesADateJournalOverrideOnItsDay(t *testing.T) {
	cal := parseICS(t, componentICS(ical.CompJournal,
		"UID:journal\nDTSTART;VALUE=DATE:20260801\nRRULE:FREQ=WEEKLY;COUNT=3\n",
		"UID:journal\nRECURRENCE-ID;VALUE=DATE:20260808\nDTSTART;VALUE=DATE:20260901\nSUMMARY:Moved\n",
	))

	out, err := limitRecurrenceCalendar(cal, &calendarTimeWindow{Start: at(2026, time.August, 8, 12, 0), End: at(2026, time.August, 8, 13, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(componentsNamed(out, ical.CompJournal)); got != 2 {
		t.Errorf("components = %d, want the master and the override whose original day covers the window", got)
	}
}
