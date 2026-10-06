package caldav_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mniehe/davkit/caldav"
)

const extraDatesICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:extra\r\nDTSTAMP:20260801T000000Z\r\n" +
	"DTSTART:20260801T090000Z\r\nDTEND:20260801T100000Z\r\nRDATE:20260820T090000Z\r\n" +
	"RDATE;VALUE=PERIOD:20260810T130000Z/PT4H\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

// RFC 4791 §7.10 reports the busy time of every instance, and an RDATE period
// is an instance with its own length.
func TestFreeBusyReportsExtraDatesAndPeriodsWithTheirOwnLength(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "extra.ics", extraDatesICS, "extra")
	h := handlerFor(t, store, caldav.Config{})

	w := report(t, h, "/alice/work/", freeBusyQuery("20260805T000000Z", "20260901T000000Z"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"FREEBUSY:20260810T130000Z/20260810T170000Z",
		"FREEBUSY:20260820T090000Z/20260820T100000Z",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if got := strings.Count(body, "FREEBUSY:"); got != 2 {
		t.Errorf("body = %q, want exactly two FREEBUSY properties, got %d", body, got)
	}
}

// RFC 4791 §7.10 counts an all-day event as busy for its whole day.
func TestFreeBusyReportsAllDayOccurrences(t *testing.T) {
	cases := map[string]string{
		"RDATE":         "DTSTART;VALUE=DATE:20260801\r\nRDATE;VALUE=DATE:20260808\r\n",
		"not recurring": "DTSTART;VALUE=DATE:20260808\r\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			store := newStore(t)
			ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:allday\r\nDTSTAMP:20260801T000000Z\r\n" +
				body + "END:VEVENT\r\nEND:VCALENDAR\r\n"
			seedRaw(t, store, "alice", "allday.ics", ics, "allday")
			h := handlerFor(t, store, caldav.Config{})

			w := report(t, h, "/alice/work/", freeBusyQuery("20260805T000000Z", "20260901T000000Z"))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusOK, w.Body.String())
			}
			if body := w.Body.String(); !strings.Contains(body, "FREEBUSY:20260808T000000Z/20260809T000000Z") {
				t.Errorf("body = %q, want 8 August busy all day", body)
			}
		})
	}
}
