package caldav_test

import (
	"net/http"
	"testing"

	"github.com/emersion/go-ical"
)

// A stored master can carry a DTSTART or DTEND that does not read, whether
// PUT let it through, as it does an unreadable DTEND, or it was stored by other
// means. A path that needs the master's interval refuses the object rather
// than failing as if the server were at fault.
func TestAMasterWhoseIntervalDoesNotReadIsRefused(t *testing.T) {
	window := func(start, end string) crossWindow { return crossWindow{start, end} }
	fixtures := map[string]*crossPaths{
		"unreadable DTEND": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:20260801T090000Z\r\nDTEND:not-a-date\r\nRRULE:FREQ=DAILY;COUNT=5\r\n",
				"UID:fixture\r\nSUMMARY:Moved\r\nRECURRENCE-ID:20260803T090000Z\r\nDTSTART:20260901T090000Z\r\nDTEND:20260901T100000Z\r\n",
			),
			put: http.StatusCreated,
			// A time-range filter reads the master's interval too, and fails on it.
			queries: []crossQuery{
				{window("20260901T093000Z", "20260901T100000Z"), true},
			},
			queryStatus:    http.StatusInternalServerError,
			expand:         window("20260801T000000Z", "20260902T000000Z"),
			expandStatus:   http.StatusForbidden,
			freeBusy:       window("20260801T000000Z", "20260902T000000Z"),
			freeBusyStatus: http.StatusForbidden,
			limits: []crossLimit{
				{window("20260901T093000Z", "20260901T100000Z"), http.StatusMultiStatus, []string{"Master", "Moved"}},
				{window("20260803T090000Z", "20260803T100000Z"), http.StatusForbidden, nil},
			},
		},
		// The master's extra dates are read against DTSTART, so the interval
		// has to be refused before they are.
		"unreadable DTSTART beside a DATE RDATE": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:not-a-date\r\nDTEND:20260801T100000Z\r\nRRULE:FREQ=WEEKLY;COUNT=5\r\nRDATE;VALUE=DATE:20260810\r\n",
				"UID:fixture\r\nSUMMARY:Moved\r\nRECURRENCE-ID:20260808T090000Z\r\nDTSTART:20260901T090000Z\r\nDTEND:20260901T100000Z\r\n",
			),
			put: http.StatusForbidden,
			queries: []crossQuery{
				{window("20260901T093000Z", "20260901T100000Z"), true},
			},
			queryStatus:    http.StatusInternalServerError,
			expand:         window("20260801T000000Z", "20260902T000000Z"),
			expandStatus:   http.StatusForbidden,
			freeBusy:       window("20260801T000000Z", "20260902T000000Z"),
			freeBusyStatus: http.StatusForbidden,
			limits: []crossLimit{
				{window("20260901T093000Z", "20260901T100000Z"), http.StatusMultiStatus, []string{"Master", "Moved"}},
				{window("20260808T090000Z", "20260808T100000Z"), http.StatusForbidden, nil},
			},
		},
	}
	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			runCrossPaths(t, fixture)
		})
	}
}
