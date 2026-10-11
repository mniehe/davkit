package caldav_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mniehe/davkit/caldav"
)

func recurringICS(lines string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:series\r\nDTSTAMP:20260801T000000Z\r\n" +
		"DTSTART:20260801T090000Z\r\nDTEND:20260801T100000Z\r\n" + lines + "END:VEVENT\r\nEND:VCALENDAR\r\n"
}

// RFC 4791 §5.3.2.1: data whose recurrence cannot be read is not valid
// calendar data, and storing it would fail every later time-range query.
func TestPutRefusesUnreadableRecurrence(t *testing.T) {
	for name, lines := range map[string]string{
		"RDATE":                 "RDATE:not-a-date\r\n",
		"RRULE":                 "RRULE:FREQ=SOMETIMES\r\n",
		"EXDATE":                "RRULE:FREQ=DAILY\r\nEXDATE:not-a-date\r\n",
		"period ending earlier": "RDATE;VALUE=PERIOD:20260810T130000Z/20260810T120000Z\r\n",
		"zero-length period":    "RDATE;VALUE=PERIOD:20260810T130000Z/20260810T130000Z\r\n",
		"zero-duration period":  "RDATE;VALUE=PERIOD:20260810T130000Z/PT0S\r\n",
		"RECURRENCE-ID":         "RECURRENCE-ID:not-a-date\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := handlerFor(t, newStore(t), caldav.Config{})

			w := put(h, "/alice/work/series.ics", recurringICS(lines), nil)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusForbidden, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "valid-calendar-data") {
				t.Errorf("body = %q, want the CALDAV:valid-calendar-data precondition", w.Body.String())
			}
		})
	}
}

func TestPutAcceptsReadableRecurrence(t *testing.T) {
	cases := map[string]string{
		"RDATE, PERIOD and EXDATE": recurringICS("RRULE:FREQ=DAILY;COUNT=3\r\nRDATE:20260810T090000Z\r\n" +
			"RDATE;VALUE=PERIOD:20260811T130000Z/PT2H,20260812T130000Z/20260812T150000Z\r\nEXDATE:20260802T090000Z\r\n"),
		// RFC 5545 §3.8.5.2 ties RDATE's value type to nothing.
		"DATE-TIME series with a DATE RDATE": recurringICS("RDATE;VALUE=DATE:20260810\r\n"),
		"DATE series with DATE-TIME and PERIOD RDATEs": "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
			"BEGIN:VEVENT\r\nUID:series\r\nDTSTAMP:20260801T000000Z\r\nDTSTART;VALUE=DATE:20260801\r\n" +
			"RDATE:20260810T090000Z\r\nRDATE;VALUE=PERIOD:20260811T130000Z/PT2H\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		// The zone is the object's own, which time.LoadLocation cannot serve.
		"embedded zone": "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
			"BEGIN:VTIMEZONE\r\nTZID:Test/Plus10\r\nBEGIN:STANDARD\r\nDTSTART:19700101T000000\r\n" +
			"TZOFFSETFROM:+1000\r\nTZOFFSETTO:+1000\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
			"BEGIN:VEVENT\r\nUID:series\r\nDTSTAMP:20260801T000000Z\r\n" +
			"DTSTART;TZID=Test/Plus10:20260801T090000\r\nDTEND;TZID=Test/Plus10:20260801T100000\r\n" +
			"RRULE:FREQ=WEEKLY\r\nRDATE;TZID=Test/Plus10;VALUE=PERIOD:20260811T090000/PT2H\r\n" +
			"EXDATE;TZID=Test/Plus10:20260808T090000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
	}
	for name, ics := range cases {
		t.Run(name, func(t *testing.T) {
			h := handlerFor(t, newStore(t), caldav.Config{})

			if w := put(h, "/alice/work/series.ics", ics, nil); w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusCreated, w.Body.String())
			}
		})
	}
}

// Data a backend already holds is answered as before: refusing it at PUT does
// not reclassify what is stored.
func TestStoredUnreadableRecurrenceStillFailsTheQuery(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "series.ics", recurringICS("RDATE:not-a-date\r\n"), "series")
	h := handlerFor(t, store, caldav.Config{})

	w := report(t, h, "/alice/work/", freeBusyQuery("20260801T000000Z", "20260901T000000Z"))
	if w.Code != http.StatusForbidden {
		t.Errorf("free-busy status = %d, want %d as before", w.Code, http.StatusForbidden)
	}
}
