package caldav

import (
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

func expandedNamed(t *testing.T, cal *ical.Calendar, name string, start, end time.Time) []*ical.Component {
	t.Helper()
	out, err := expandCalendar(cal, &calendarExpandRequest{Start: start, End: end})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	return componentsNamed(out, name)
}

// RFC 5545 §3.8.4.4: an override replaces the instance its RECURRENCE-ID
// names, so the instance no longer occupies its original time.
func TestMovedOccurrenceIsFoundAtItsNewTimeOnly(t *testing.T) {
	cases := []struct {
		name         string
		master       string
		recurrenceID string
		original     span
		moved        span
		movedDTStart string
		movedDTEnd   string
	}{
		{
			name:         "RRULE occurrence",
			master:       "DTSTART:20260801T090000Z\nDTEND:20260801T100000Z\nRRULE:FREQ=DAILY;COUNT=5\n",
			recurrenceID: "20260803T090000Z",
			original:     span{at(2026, time.August, 3, 9, 0), at(2026, time.August, 3, 10, 0)},
			moved:        span{at(2026, time.August, 3, 15, 0), at(2026, time.August, 3, 16, 0)},
			movedDTStart: "20260803T150000Z",
			movedDTEnd:   "20260803T160000Z",
		},
		{
			name:         "RDATE occurrence",
			master:       "DTSTART:20260801T090000Z\nDTEND:20260801T100000Z\nRDATE:20260810T090000Z\n",
			recurrenceID: "20260810T090000Z",
			original:     span{at(2026, time.August, 10, 9, 0), at(2026, time.August, 10, 10, 0)},
			moved:        span{at(2026, time.August, 10, 15, 0), at(2026, time.August, 10, 16, 0)},
			movedDTStart: "20260810T150000Z",
			movedDTEnd:   "20260810T160000Z",
		},
		{
			name:         "PERIOD occurrence",
			master:       "DTSTART:20260801T090000Z\nDTEND:20260801T100000Z\nRDATE;VALUE=PERIOD:20260810T130000Z/PT4H\n",
			recurrenceID: "20260810T130000Z",
			// 15:00 is past the master's own hour from 13:00, so only the
			// period reaches it.
			original:     span{at(2026, time.August, 10, 15, 0), at(2026, time.August, 10, 16, 0)},
			moved:        span{at(2026, time.August, 12, 9, 0), at(2026, time.August, 12, 10, 0)},
			movedDTStart: "20260812T090000Z",
			movedDTEnd:   "20260812T100000Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cal := parseICS(t, eventICS(
				"UID:moved\nSUMMARY:Master\n"+tc.master,
				"UID:moved\nSUMMARY:Moved\nRECURRENCE-ID:"+tc.recurrenceID+"\nDTSTART:"+tc.movedDTStart+"\nDTEND:"+tc.movedDTEnd+"\n",
			))

			if matchesWindow(t, cal, tc.original.start, tc.original.end) {
				t.Error("a window over the moved occurrence's original time matched")
			}
			if !matchesWindow(t, cal, tc.moved.start, tc.moved.end) {
				t.Error("a window over the moved occurrence's new time did not match")
			}
			assertSpans(t, expandedSpans(t, cal, tc.original.start, tc.original.end))
			assertSpans(t, expandedSpans(t, cal, tc.moved.start, tc.moved.end), tc.moved)
		})
	}
}

// RFC 4791 §9.9 measures a VTODO by DTSTART and DUE, so a window inside the
// original DTSTART..DUE is the original time, and one inside the override's
// DTSTART..DUE is the new time even though neither holds its DTSTART.
func TestMovedTodoOccurrenceIsMeasuredByItsDue(t *testing.T) {
	cal := parseICS(t, componentICS(ical.CompToDo,
		"UID:todo\nSUMMARY:Master\nDTSTART:20260801T090000Z\nDUE:20260801T170000Z\nRRULE:FREQ=DAILY;COUNT=5\n",
		"UID:todo\nSUMMARY:Moved\nRECURRENCE-ID:20260803T090000Z\nDTSTART:20260803T180000Z\nDUE:20260803T200000Z\n",
	))
	originalFrom, originalTo := at(2026, time.August, 3, 12, 0), at(2026, time.August, 3, 13, 0)
	movedFrom, movedTo := at(2026, time.August, 3, 19, 0), at(2026, time.August, 3, 19, 30)

	if matchesKind(t, cal, ical.CompToDo, originalFrom, originalTo) {
		t.Error("a window inside the original DTSTART..DUE matched")
	}
	if !matchesKind(t, cal, ical.CompToDo, movedFrom, movedTo) {
		t.Error("a window inside the override's DTSTART..DUE did not match")
	}
	if got := expandedNamed(t, cal, ical.CompToDo, originalFrom, originalTo); len(got) != 0 {
		t.Errorf("expanding the original time gave %d instances, want none", len(got))
	}
	todos := expandedNamed(t, cal, ical.CompToDo, movedFrom, movedTo)
	if len(todos) != 1 {
		t.Fatalf("expanding the new time gave %d instances, want the override once", len(todos))
	}
	assertProp(t, todos[0], ical.PropSummary, ical.ValueText, "Moved")
	assertProp(t, todos[0], ical.PropDateTimeStart, ical.ValueDateTime, "20260803T180000Z")
}
