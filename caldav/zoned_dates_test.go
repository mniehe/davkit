package caldav_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/emersion/go-ical"
	"github.com/mniehe/davkit/caldav"
)

const (
	newYork = "America/New_York"
	// privateEastern names the zone easternVTimezone defines, which
	// time.LoadLocation cannot serve.
	privateEastern = "/example.test/Eastern"
)

// easternVTimezone is US Eastern under a private TZID, so only the object's
// own definition can place a time in it.
const easternVTimezone = "BEGIN:VTIMEZONE\r\nTZID:" + privateEastern + "\r\n" +
	"BEGIN:DAYLIGHT\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0400\r\nTZNAME:EDT\r\n" +
	"DTSTART:20070311T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU\r\nEND:DAYLIGHT\r\n" +
	"BEGIN:STANDARD\r\nTZOFFSETFROM:-0400\r\nTZOFFSETTO:-0500\r\nTZNAME:EST\r\n" +
	"DTSTART:20071104T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\r\nEND:STANDARD\r\n" +
	"END:VTIMEZONE\r\n"

// zonedCalendar is eventCalendar with vtimezone placed before the events.
func zonedCalendar(vtimezone string, bodies ...string) string {
	ics := eventCalendar(bodies...)
	if vtimezone == "" {
		return ics
	}
	head, tail, _ := strings.Cut(ics, "BEGIN:VEVENT")
	return head + vtimezone + "BEGIN:VEVENT" + tail
}

// zonedMaster is 09:00-10:00 on 2026-08-01 in tzid, 13:00-14:00Z in August,
// with extra appended to its properties.
func zonedMaster(tzid, extra string) string {
	return "UID:fixture\r\nSUMMARY:Master\r\n" +
		"DTSTART;TZID=" + tzid + ":20260801T090000\r\nDTEND;TZID=" + tzid + ":20260801T100000\r\n" + extra
}

// RFC 5545 §3.8.5.2 lets an RDATE or EXDATE be a DATE on a DATE-TIME series;
// the date then names its midnight in the zone DTSTART is written in. In
// August New York is four hours behind UTC, so 2026-08-08 starts at 04:00Z,
// and every path must place the occurrence there.
func TestADateAmongATimedSeriesExtraDatesIsMidnightInItsZone(t *testing.T) {
	window := func(start, end string) crossWindow { return crossWindow{start, end} }
	zones := map[string]struct{ tzid, vtimezone, recurrenceID string }{
		"IANA zone": {newYork, "", "RECURRENCE-ID:20260808T040000Z\r\n"},
		"embedded VTIMEZONE": {
			privateEastern, easternVTimezone,
			"RECURRENCE-ID;TZID=" + privateEastern + ":20260808T000000\r\n",
		},
	}
	for name, zone := range zones {
		t.Run(name+", DATE RDATE", func(t *testing.T) {
			runCrossPaths(t, &crossPaths{
				comp: ical.CompEvent,
				ics:  zonedCalendar(zone.vtimezone, zonedMaster(zone.tzid, "RDATE;VALUE=DATE:20260808\r\n")),
				put:  http.StatusCreated,
				queries: []crossQuery{
					{window("20260808T040000Z", "20260808T050000Z"), true},
					{window("20260808T000000Z", "20260808T010000Z"), false},
				},
				expand:       window("20260808T000000Z", "20260809T000000Z"),
				expandStatus: http.StatusMultiStatus,
				instances: []crossInstance{{
					ical.PropDateTimeStart: "DATE-TIME:20260808T040000Z",
					ical.PropRecurrenceID:  "DATE-TIME:20260808T040000Z",
					ical.PropDateTimeEnd:   "DATE-TIME:20260808T050000Z",
				}},
				freeBusy:       window("20260805T000000Z", "20260810T000000Z"),
				freeBusyStatus: http.StatusOK,
				busy:           []string{"20260808T040000Z/20260808T050000Z"},
				limits: []crossLimit{
					{window("20260801T000000Z", "20260901T000000Z"), http.StatusMultiStatus, []string{"Master"}},
				},
			})
		})

		t.Run(name+", DATE RDATE overridden", func(t *testing.T) {
			runCrossPaths(t, &crossPaths{
				comp: ical.CompEvent,
				ics: zonedCalendar(zone.vtimezone,
					zonedMaster(zone.tzid, "RDATE;VALUE=DATE:20260808\r\n"),
					"UID:fixture\r\nSUMMARY:Moved\r\n"+zone.recurrenceID+"DTSTART:20260901T090000Z\r\nDTEND:20260901T100000Z\r\n",
				),
				put: http.StatusCreated,
				queries: []crossQuery{
					{window("20260808T040000Z", "20260808T050000Z"), false},
					{window("20260808T000000Z", "20260808T010000Z"), false},
					{window("20260901T093000Z", "20260901T100000Z"), true},
				},
				expand:       window("20260801T000000Z", "20260902T000000Z"),
				expandStatus: http.StatusMultiStatus,
				instances: []crossInstance{
					{
						ical.PropDateTimeStart: "DATE-TIME:20260801T130000Z",
						ical.PropRecurrenceID:  "DATE-TIME:20260801T130000Z",
					},
					{
						ical.PropSummary:       "Moved",
						ical.PropDateTimeStart: "DATE-TIME:20260901T090000Z",
						ical.PropRecurrenceID:  "DATE-TIME:20260808T040000Z",
					},
				},
				freeBusy:       window("20260805T000000Z", "20260905T000000Z"),
				freeBusyStatus: http.StatusOK,
				busy:           []string{"20260901T090000Z/20260901T100000Z"},
				limits: []crossLimit{
					{window("20260808T040000Z", "20260808T050000Z"), http.StatusMultiStatus, []string{"Master", "Moved"}},
					{window("20260808T000000Z", "20260808T010000Z"), http.StatusMultiStatus, []string{"Master"}},
				},
			})
		})
	}

	// Whatever the RDATE is, the DATE EXDATE takes away the occurrence at the
	// zone's midnight and nothing at UTC's.
	for zoneName, zone := range zones {
		excluded := map[string]string{
			"DATE RDATE":                  "RDATE;VALUE=DATE:20260808\r\nEXDATE;VALUE=DATE:20260808\r\n",
			"DATE-TIME RDATE at midnight": "RDATE;TZID=" + zone.tzid + ":20260808T000000\r\nEXDATE;VALUE=DATE:20260808\r\n",
		}
		for name, extra := range excluded {
			t.Run(zoneName+", DATE EXDATE, "+name, func(t *testing.T) {
				runCrossPaths(t, &crossPaths{
					comp: ical.CompEvent,
					ics:  zonedCalendar(zone.vtimezone, zonedMaster(zone.tzid, extra)),
					put:  http.StatusCreated,
					queries: []crossQuery{
						{window("20260808T040000Z", "20260808T050000Z"), false},
						{window("20260808T000000Z", "20260808T010000Z"), false},
					},
					expand:       window("20260801T000000Z", "20260809T000000Z"),
					expandStatus: http.StatusMultiStatus,
					instances: []crossInstance{{
						ical.PropDateTimeStart: "DATE-TIME:20260801T130000Z",
					}},
					freeBusy:       window("20260801T000000Z", "20260810T000000Z"),
					freeBusyStatus: http.StatusOK,
					busy:           []string{"20260801T130000Z/20260801T140000Z"},
					limits: []crossLimit{
						{window("20260801T000000Z", "20260901T000000Z"), http.StatusMultiStatus, []string{"Master"}},
					},
				})
			})
		}

		t.Run(zoneName+", DATE EXDATE on a rule", func(t *testing.T) {
			runCrossPaths(t, &crossPaths{
				comp: ical.CompEvent,
				ics: zonedCalendar(zone.vtimezone, "UID:fixture\r\nSUMMARY:Master\r\n"+
					"DTSTART;TZID="+zone.tzid+":20260801T000000\r\nDTEND;TZID="+zone.tzid+":20260801T010000\r\n"+
					"RRULE:FREQ=DAILY;COUNT=3\r\nEXDATE;VALUE=DATE:20260802\r\n"),
				put: http.StatusCreated,
				queries: []crossQuery{
					{window("20260802T040000Z", "20260802T050000Z"), false},
					{window("20260803T040000Z", "20260803T050000Z"), true},
				},
				expand:       window("20260801T000000Z", "20260804T000000Z"),
				expandStatus: http.StatusMultiStatus,
				instances: []crossInstance{
					{ical.PropDateTimeStart: "DATE-TIME:20260801T040000Z"},
					{ical.PropDateTimeStart: "DATE-TIME:20260803T040000Z"},
				},
				freeBusy:       window("20260801T000000Z", "20260804T000000Z"),
				freeBusyStatus: http.StatusOK,
				busy:           []string{"20260801T040000Z/20260801T050000Z", "20260803T040000Z/20260803T050000Z"},
				limits: []crossLimit{
					{window("20260801T000000Z", "20260901T000000Z"), http.StatusMultiStatus, []string{"Master"}},
				},
			})
		})
	}

	// The period index limit-recurrence-set reads an original's end from
	// applies the same EXDATE instant, so the excluded period lends the
	// override no end.
	t.Run("DATE EXDATE, PERIOD RDATE overridden", func(t *testing.T) {
		runCrossPaths(t, &crossPaths{
			comp: ical.CompEvent,
			ics: eventCalendar(
				zonedMaster(newYork, "RDATE;VALUE=PERIOD:20260808T040000Z/PT3H\r\nEXDATE;VALUE=DATE:20260808\r\n"),
				"UID:fixture\r\nSUMMARY:Moved\r\nRECURRENCE-ID:20260808T040000Z\r\nDTSTART:20260901T090000Z\r\nDTEND:20260901T100000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260808T050000Z", "20260808T060000Z"), false},
				{window("20260901T093000Z", "20260901T100000Z"), true},
			},
			expand:       window("20260801T000000Z", "20260902T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{
				{ical.PropDateTimeStart: "DATE-TIME:20260801T130000Z"},
				{ical.PropSummary: "Moved", ical.PropDateTimeStart: "DATE-TIME:20260901T090000Z"},
			},
			freeBusy:       window("20260805T000000Z", "20260905T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260901T090000Z/20260901T100000Z"},
			limits: []crossLimit{
				{window("20260808T050000Z", "20260808T060000Z"), http.StatusMultiStatus, []string{"Master"}},
				{window("20260808T040000Z", "20260808T050000Z"), http.StatusMultiStatus, []string{"Master", "Moved"}},
			},
		})
	})

	t.Run("UTC series, DATE RDATE", func(t *testing.T) {
		runCrossPaths(t, &crossPaths{
			comp: ical.CompEvent,
			ics: eventCalendar("UID:fixture\r\nSUMMARY:Master\r\n" +
				"DTSTART:20260801T090000Z\r\nDTEND:20260801T100000Z\r\nRDATE;VALUE=DATE:20260808\r\n"),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260808T000000Z", "20260808T010000Z"), true},
				{window("20260808T040000Z", "20260808T050000Z"), false},
			},
			expand:       window("20260808T000000Z", "20260809T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{{
				ical.PropDateTimeStart: "DATE-TIME:20260808T000000Z",
				ical.PropRecurrenceID:  "DATE-TIME:20260808T000000Z",
				ical.PropDateTimeEnd:   "DATE-TIME:20260808T010000Z",
			}},
			freeBusy:       window("20260805T000000Z", "20260810T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260808T000000Z/20260808T010000Z"},
			limits: []crossLimit{
				{window("20260801T000000Z", "20260901T000000Z"), http.StatusMultiStatus, []string{"Master"}},
			},
		})
	})
}

// A floating DTSTART is read in the zone a query supplies, and so is the
// midnight of a DATE among its extra dates.
func TestADateAmongAFloatingSeriesExtraDatesIsMidnightInTheQueryZone(t *testing.T) {
	const ics = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:floating\r\nDTSTAMP:20260801T000000Z\r\n" +
		"DTSTART:20260801T090000\r\nDTEND:20260801T100000\r\nRDATE;VALUE=DATE:20260808\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	store := newStore(t)
	seedRaw(t, store, "alice", "floating.ics", ics, "floating")
	h := handlerFor(t, store, caldav.Config{})

	ms := reportMS(t, h, "/alice/work/", timeRangeQueryTZ("20260808T040000Z", "20260808T050000Z", easternTimezoneSpec))
	if got := ms.hrefs(); len(got) != 1 || got[0] != "/alice/work/floating.ics" {
		t.Fatalf("hrefs = %v, want the occurrence at Eastern midnight, 04:00Z", got)
	}
	data := ms.at(t, "/alice/work/floating.ics").value(t, caldavName("calendar-data"))
	if !strings.Contains(data, "RDATE;VALUE=DATE:20260808") {
		t.Errorf("calendar-data no longer carries the stored RDATE:\n%s", data)
	}

	ms = reportMS(t, h, "/alice/work/", timeRangeQueryTZ("20260808T000000Z", "20260808T010000Z", easternTimezoneSpec))
	if got := ms.hrefs(); len(got) != 0 {
		t.Fatalf("hrefs = %v, want no occurrence at UTC midnight", got)
	}
}
