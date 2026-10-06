package caldav

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/mniehe/davkit/internal"
)

// eventICS wraps VEVENT bodies in a VCALENDAR. Each body is a list of content
// lines without the BEGIN/END pair.
func eventICS(bodies ...string) string {
	ics := "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//EN\n"
	for _, body := range bodies {
		ics += "BEGIN:VEVENT\nDTSTAMP:20260101T000000Z\n" + body + "END:VEVENT\n"
	}
	return ics + "END:VCALENDAR\n"
}

type span struct{ start, end time.Time }

func at(y int, mo time.Month, d, h, m int) time.Time {
	return time.Date(y, mo, d, h, m, 0, 0, time.UTC)
}

// expandedSpans expands cal over [start, end) and returns each instance's
// DTSTART and DTEND as written.
func expandedSpans(t *testing.T, cal *ical.Calendar, start, end time.Time) []span {
	t.Helper()
	out, err := expandCalendar(cal, &calendarExpandRequest{Start: start, End: end})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	events := componentsNamed(out, ical.CompEvent)
	spans := make([]span, 0, len(events))
	for _, ev := range events {
		from, ferr := ev.Props.DateTime(ical.PropDateTimeStart, time.UTC)
		to, terr := ev.Props.DateTime(ical.PropDateTimeEnd, time.UTC)
		if ferr != nil || terr != nil {
			t.Fatalf("instance times: %v, %v", ferr, terr)
		}
		spans = append(spans, span{from, to})
	}
	return spans
}

func assertSpans(t *testing.T, got []span, want ...span) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("instances = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].start.Equal(want[i].start) || !got[i].end.Equal(want[i].end) {
			t.Errorf("instance %d = %v..%v, want %v..%v", i, got[i].start, got[i].end, want[i].start, want[i].end)
		}
	}
}

func matchesWindow(t *testing.T, cal *ical.Calendar, start, end time.Time) bool {
	t.Helper()
	ok, err := match(vevent(ical.CompEvent, start, end), cal.Component)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	return ok
}

const rdateOnly = "UID:rdate-only\nDTSTART:20260801T090000Z\nDTEND:20260801T100000Z\n" +
	"RDATE:20260810T090000Z,20260820T090000Z\n"

// RFC 5545 §3.8.5.2: RDATE adds occurrences to the set whether or not an RRULE
// is present.
func TestRDateOnlyOccurrenceMatchesAWindowExcludingDTStart(t *testing.T) {
	cal := parseICS(t, eventICS(rdateOnly))

	if !matchesWindow(t, cal, at(2026, time.August, 20, 9, 30), at(2026, time.August, 20, 9, 45)) {
		t.Error("an RDATE occurrence inside the window did not match")
	}
	if matchesWindow(t, cal, at(2026, time.August, 15, 0, 0), at(2026, time.August, 16, 0, 0)) {
		t.Error("a window between occurrences matched")
	}
}

func TestRDateOnlyOccurrenceExpandsWithTheComponentsLength(t *testing.T) {
	cal := parseICS(t, eventICS(rdateOnly))

	spans := expandedSpans(t, cal, at(2026, time.August, 15, 0, 0), at(2026, time.August, 25, 0, 0))

	assertSpans(t, spans, span{at(2026, time.August, 20, 9, 0), at(2026, time.August, 20, 10, 0)})
}

func TestRDateOnlyExpansionGivesDTStartARecurrenceID(t *testing.T) {
	cal := parseICS(t, eventICS(rdateOnly))

	out, err := expandCalendar(cal, &calendarExpandRequest{Start: at(2026, time.August, 1, 0, 0), End: at(2026, time.August, 2, 0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	events := componentsNamed(out, ical.CompEvent)
	if len(events) != 1 {
		t.Fatalf("instances = %d, want 1", len(events))
	}
	if events[0].Props.Get(ical.PropRecurrenceDates) != nil {
		t.Error("the DTSTART instance still carries RDATE")
	}
	rid, err := events[0].Props.DateTime(ical.PropRecurrenceID, time.UTC)
	if err != nil || !rid.Equal(at(2026, time.August, 1, 9, 0)) {
		t.Errorf("RECURRENCE-ID = %v (%v), want the DTSTART", rid, err)
	}
}

func TestRDateEarlierThanDTStartIsFound(t *testing.T) {
	cal := parseICS(t, eventICS("UID:early\nDTSTART:20260901T090000Z\nDTEND:20260901T100000Z\nRDATE:20260801T090000Z\n"))
	from, to := at(2026, time.August, 1, 0, 0), at(2026, time.August, 2, 0, 0)

	if !matchesWindow(t, cal, from, to) {
		t.Error("an RDATE before DTSTART did not match")
	}
	assertSpans(t, expandedSpans(t, cal, from, to), span{at(2026, time.August, 1, 9, 0), at(2026, time.August, 1, 10, 0)})
}

// RFC 5545 §3.3.9 gives a period two forms; both carry their own length.
func TestPeriodOccurrenceKeepsItsOwnEnd(t *testing.T) {
	for name, value := range map[string]string{
		"explicit end": "20260810T130000Z/20260810T170000Z",
		"duration":     "20260810T130000Z/PT4H",
	} {
		t.Run(name, func(t *testing.T) {
			cal := parseICS(t, eventICS("UID:period\nDTSTART:20260801T090000Z\nDTEND:20260801T100000Z\nRDATE;VALUE=PERIOD:"+value+"\n"))

			// 15:00 is past the master's one hour from 13:00, so only the
			// period's own end reaches it.
			if !matchesWindow(t, cal, at(2026, time.August, 10, 15, 0), at(2026, time.August, 10, 16, 0)) {
				t.Error("a window inside the period did not match")
			}
			if matchesWindow(t, cal, at(2026, time.August, 10, 17, 0), at(2026, time.August, 10, 18, 0)) {
				t.Error("a window after the period's end matched")
			}
			spans := expandedSpans(t, cal, at(2026, time.August, 10, 0, 0), at(2026, time.August, 11, 0, 0))
			assertSpans(t, spans, span{at(2026, time.August, 10, 13, 0), at(2026, time.August, 10, 17, 0)})
		})
	}
}

func TestPeriodInstanceCarriesNoDuration(t *testing.T) {
	cal := parseICS(t, eventICS("UID:period-dur\nDTSTART:20260801T090000Z\nDURATION:PT1H\nRDATE;VALUE=PERIOD:20260810T130000Z/PT4H\n"))

	spans := expandedSpans(t, cal, at(2026, time.August, 10, 0, 0), at(2026, time.August, 11, 0, 0))
	assertSpans(t, spans, span{at(2026, time.August, 10, 13, 0), at(2026, time.August, 10, 17, 0)})

	out, err := expandCalendar(cal, &calendarExpandRequest{Start: at(2026, time.August, 10, 0, 0), End: at(2026, time.August, 11, 0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if componentsNamed(out, ical.CompEvent)[0].Props.Get(ical.PropDuration) != nil {
		t.Error("a period instance carries both DTEND and the master's DURATION")
	}
}

const ruleAndPeriod = "UID:rule-period\nDTSTART:20260801T090000Z\nDTEND:20260801T100000Z\n" +
	"RRULE:FREQ=DAILY;COUNT=3\nRDATE;VALUE=PERIOD:20260805T130000Z/PT2H\n"

func TestRuleBesidePeriodYieldsBoth(t *testing.T) {
	cal := parseICS(t, eventICS(ruleAndPeriod))

	spans := expandedSpans(t, cal, at(2026, time.August, 1, 0, 0), at(2026, time.August, 6, 0, 0))
	assertSpans(t, spans,
		span{at(2026, time.August, 1, 9, 0), at(2026, time.August, 1, 10, 0)},
		span{at(2026, time.August, 2, 9, 0), at(2026, time.August, 2, 10, 0)},
		span{at(2026, time.August, 3, 9, 0), at(2026, time.August, 3, 10, 0)},
		span{at(2026, time.August, 5, 13, 0), at(2026, time.August, 5, 15, 0)},
	)
	if !matchesWindow(t, cal, at(2026, time.August, 5, 14, 0), at(2026, time.August, 5, 14, 30)) {
		t.Error("a window inside the period did not match")
	}
	if !matchesWindow(t, cal, at(2026, time.August, 2, 9, 30), at(2026, time.August, 2, 9, 45)) {
		t.Error("a window inside a rule occurrence did not match")
	}
}

func TestExDateRemovesPlainAndPeriodOccurrences(t *testing.T) {
	cal := parseICS(t, eventICS("UID:exdate\nDTSTART:20260801T090000Z\nDTEND:20260801T100000Z\n"+
		"RDATE:20260810T090000Z,20260812T090000Z\nRDATE;VALUE=PERIOD:20260811T130000Z/PT2H\n"+
		"EXDATE:20260801T090000Z,20260810T090000Z,20260811T130000Z\n"))

	spans := expandedSpans(t, cal, at(2026, time.August, 1, 0, 0), at(2026, time.August, 13, 0, 0))
	assertSpans(t, spans, span{at(2026, time.August, 12, 9, 0), at(2026, time.August, 12, 10, 0)})

	if matchesWindow(t, cal, at(2026, time.August, 1, 0, 0), at(2026, time.August, 12, 0, 0)) {
		t.Error("a window holding only excluded occurrences matched")
	}
	if !matchesWindow(t, cal, at(2026, time.August, 12, 0, 0), at(2026, time.August, 13, 0, 0)) {
		t.Error("the occurrence left after EXDATE did not match")
	}
}

// RFC 5545 §3.8.5.2 does not say which length wins when dates share a start.
// A period states one, so it beats a plain date, and the longer of two periods
// is kept since either way the time is taken.
func TestSharedStartCollapsesToOneOccurrenceWithThePeriodsLength(t *testing.T) {
	cal := parseICS(t, eventICS("UID:dup\nDTSTART:20260801T090000Z\nDTEND:20260801T100000Z\n"+
		"RDATE:20260801T090000Z,20260810T090000Z\nRDATE;VALUE=PERIOD:20260801T090000Z/PT3H\n"+
		"RDATE;VALUE=PERIOD:20260810T090000Z/PT30M\nRDATE;VALUE=PERIOD:20260801T090000Z/PT2H\n"))

	spans := expandedSpans(t, cal, at(2026, time.August, 1, 0, 0), at(2026, time.August, 11, 0, 0))
	assertSpans(t, spans,
		span{at(2026, time.August, 1, 9, 0), at(2026, time.August, 1, 12, 0)},
		span{at(2026, time.August, 10, 9, 0), at(2026, time.August, 10, 9, 30)},
	)
}

// Which date is listed first must not decide the length kept.
func TestSharedStartLengthDoesNotDependOnListingOrder(t *testing.T) {
	const (
		plain  = "RDATE:20260810T090000Z\n"
		short  = "RDATE;VALUE=PERIOD:20260810T090000Z/PT30M\n"
		long   = "RDATE;VALUE=PERIOD:20260810T090000Z/PT3H\n"
		middle = "RDATE;VALUE=PERIOD:20260810T090000Z/PT2H\n"
	)
	cases := []struct {
		name  string
		dates string
		end   time.Time
	}{
		{"plain before period", plain + short, at(2026, time.August, 10, 9, 30)},
		{"period before plain", short + plain, at(2026, time.August, 10, 9, 30)},
		{"shorter period first", middle + long, at(2026, time.August, 10, 12, 0)},
		{"longer period first", long + middle, at(2026, time.August, 10, 12, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cal := parseICS(t, eventICS("UID:order\nDTSTART:20260801T090000Z\nDTEND:20260801T100000Z\n"+tc.dates))

			spans := expandedSpans(t, cal, at(2026, time.August, 10, 0, 0), at(2026, time.August, 11, 0, 0))
			assertSpans(t, spans, span{at(2026, time.August, 10, 9, 0), tc.end})
		})
	}
}

func TestOverrideReplacesAnRDateOccurrence(t *testing.T) {
	cal := parseICS(t, eventICS(
		"UID:ov\nDTSTART:20260801T090000Z\nDTEND:20260801T100000Z\nRDATE:20260810T090000Z\nSUMMARY:Master\n",
		"UID:ov\nRECURRENCE-ID:20260810T090000Z\nDTSTART:20260810T110000Z\nDTEND:20260810T120000Z\nSUMMARY:Moved\n",
	))

	spans := expandedSpans(t, cal, at(2026, time.August, 1, 0, 0), at(2026, time.August, 11, 0, 0))
	assertSpans(t, spans,
		span{at(2026, time.August, 1, 9, 0), at(2026, time.August, 1, 10, 0)},
		span{at(2026, time.August, 10, 11, 0), at(2026, time.August, 10, 12, 0)},
	)
}

func TestOverrideReplacesAPeriodOccurrence(t *testing.T) {
	cal := parseICS(t, eventICS(
		ruleAndPeriod+"SUMMARY:Master\n",
		"UID:rule-period\nRECURRENCE-ID:20260805T130000Z\nDTSTART:20260805T180000Z\nDTEND:20260805T190000Z\nSUMMARY:Moved\n",
	))

	spans := expandedSpans(t, cal, at(2026, time.August, 5, 0, 0), at(2026, time.August, 6, 0, 0))
	assertSpans(t, spans, span{at(2026, time.August, 5, 18, 0), at(2026, time.August, 5, 19, 0)})
}

// RFC 4791 §9.6.6 judges an override on its original time too, and the
// original of a period occurrence is the period, not the master's length.
func TestLimitRecurrenceSetJudgesAPeriodOverrideOnThePeriod(t *testing.T) {
	cal := parseICS(t, eventICS(
		"UID:lim\nDTSTART:20260801T090000Z\nDTEND:20260801T100000Z\nRDATE;VALUE=PERIOD:20260810T130000Z/PT4H\n",
		"UID:lim\nRECURRENCE-ID:20260810T130000Z\nDTSTART:20260901T090000Z\nDTEND:20260901T100000Z\nSUMMARY:Moved\n",
	))

	out, err := limitRecurrenceCalendar(cal, &calendarTimeWindow{Start: at(2026, time.August, 10, 15, 0), End: at(2026, time.August, 10, 16, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(componentsNamed(out, ical.CompEvent)); got != 2 {
		t.Errorf("components = %d, want the master and the override whose original period covers the window", got)
	}
}

const plus10Zone = "BEGIN:VTIMEZONE\nTZID:Test/Plus10\nBEGIN:STANDARD\nDTSTART:19700101T000000\n" +
	"TZOFFSETFROM:+1000\nTZOFFSETTO:+1000\nEND:STANDARD\nEND:VTIMEZONE\n"

// RFC 5545 §3.8.5.2 lets RDATE carry a TZID, read as DTSTART's is.
func TestZonedRecurrenceDates(t *testing.T) {
	cases := map[string]string{
		"IANA zone": eventICS("UID:ny\nDTSTART;TZID=America/New_York:20260801T090000\nDTEND;TZID=America/New_York:20260801T100000\n" +
			"RDATE;TZID=America/New_York;VALUE=PERIOD:20260810T130000/PT4H\nRDATE;TZID=America/New_York:20260820T090000\n"),
		"embedded zone": strings.Replace(eventICS("UID:emb\nDTSTART;TZID=Test/Plus10:20260801T090000\nDTEND;TZID=Test/Plus10:20260801T100000\n"+
			"RDATE;TZID=Test/Plus10;VALUE=PERIOD:20260811T030000/20260811T070000\nRDATE;TZID=Test/Plus10:20260820T230000\n"),
			"PRODID:-//test//EN\n", "PRODID:-//test//EN\n"+plus10Zone, 1),
	}
	for name, ics := range cases {
		t.Run(name, func(t *testing.T) {
			cal := parseICS(t, ics)

			spans := expandedSpans(t, cal, at(2026, time.August, 10, 0, 0), at(2026, time.August, 21, 0, 0))
			assertSpans(t, spans,
				span{at(2026, time.August, 10, 17, 0), at(2026, time.August, 10, 21, 0)},
				span{at(2026, time.August, 20, 13, 0), at(2026, time.August, 20, 14, 0)},
			)

			resolved, err := resolveObjectTimes(cal, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !matchesWindow(t, resolved, at(2026, time.August, 10, 20, 0), at(2026, time.August, 10, 20, 30)) {
				t.Error("a window inside the zoned period did not match")
			}
		})
	}
}

// A floating RDATE is read in the zone the query supplies, as a floating
// DTSTART is.
func TestFloatingPeriodIsReadInTheQueryZone(t *testing.T) {
	cal := parseICS(t, eventICS("UID:float\nDTSTART:20260801T090000\nDTEND:20260801T100000\n"+
		"RDATE;VALUE=PERIOD:20260810T130000/20260810T170000\n"))
	zone, err := resolverFromICS([]byte("BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//EN\n" + plus10Zone + "END:VCALENDAR\n"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveObjectTimes(cal, zone)
	if err != nil {
		t.Fatal(err)
	}

	// 13:00-17:00 at +10:00 is 03:00-07:00 UTC.
	if !matchesWindow(t, resolved, at(2026, time.August, 10, 6, 0), at(2026, time.August, 10, 6, 30)) {
		t.Error("a window inside the floating period, read at +10:00, did not match")
	}
	if matchesWindow(t, resolved, at(2026, time.August, 10, 14, 0), at(2026, time.August, 10, 15, 0)) {
		t.Error("the floating period was read as UTC")
	}
}

// A period that does not parse, or does not end after it starts (RFC 5545
// §3.3.9), fails the way an unreadable RRULE does in each path: match returns
// the error, expansion refuses with 403.
func TestUnreadablePeriodFailsLikeAnUnreadableRule(t *testing.T) {
	for name, value := range map[string]string{
		"garbage":           "20260810T130000Z/tomorrow",
		"end before start":  "20260810T130000Z/20260810T120000Z",
		"negative duration": "20260810T130000Z/-PT1H",
		"zero length":       "20260810T130000Z/20260810T130000Z",
		"zero duration":     "20260810T130000Z/PT0S",
	} {
		t.Run(name, func(t *testing.T) {
			cal := parseICS(t, eventICS("UID:bad\nDTSTART:20260801T090000Z\nDTEND:20260801T100000Z\nRDATE;VALUE=PERIOD:"+value+"\n"))

			if _, err := match(vevent(ical.CompEvent, at(2026, time.August, 1, 0, 0), at(2026, time.September, 1, 0, 0)), cal.Component); err == nil {
				t.Error("match accepted an unreadable period")
			}
			_, err := expandCalendar(cal, &calendarExpandRequest{Start: at(2026, time.August, 1, 0, 0), End: at(2026, time.September, 1, 0, 0)})
			var httpErr *internal.HTTPError
			if !errors.As(err, &httpErr) || httpErr.Code != http.StatusForbidden {
				t.Errorf("expand err = %v, want 403", err)
			}
		})
	}
}

// One RDATE or EXDATE property can list any number of dates; reading them is
// work the same cap as the walk bounds.
func TestHugeRDateListIsBoundedByTheIterationCap(t *testing.T) {
	values := make([]string, maxRecurrenceIterations+1)
	for i := range values {
		values[i] = at(2026, time.January, 1, 0, 0).Add(time.Duration(i) * time.Minute).Format(dateWithUTCTimeLayout)
	}
	list := strings.Join(values, ",")
	objects := map[string]*ical.Calendar{
		"RDATE":                parseICS(t, eventICS("UID:huge\nDTSTART:20260101T000000Z\nDTEND:20260101T000100Z\nRDATE:"+list+"\n")),
		"EXDATE":               parseICS(t, eventICS("UID:huge\nDTSTART:20260101T000000Z\nDTEND:20260101T000100Z\nRDATE:20300101T000000Z\nEXDATE:"+list+"\n")),
		"EXDATE beside a rule": parseICS(t, eventICS("UID:huge\nDTSTART:20260101T000000Z\nDTEND:20260101T000100Z\nRRULE:FREQ=MINUTELY\nEXDATE:"+list+"\n")),
	}

	// A window after every date is reached only by walking them all; one before
	// the first is answered without a step, so only reading the list can trip
	// the cap there.
	windows := map[string]time.Time{
		"after every date":  at(2100, time.January, 1, 0, 0),
		"before every date": at(2025, time.January, 1, 0, 0),
	}
	for list, cal := range objects {
		for name, from := range windows {
			t.Run(list+" "+name, func(t *testing.T) {
				_, err := match(vevent(ical.CompEvent, from, from.Add(time.Hour)), cal.Component)
				var httpErr *internal.HTTPError
				if !errors.As(err, &httpErr) || httpErr.Code != http.StatusInsufficientStorage {
					t.Errorf("match err = %v, want 507", err)
				}

				_, err = expandCalendar(cal, &calendarExpandRequest{Start: from, End: from.Add(time.Hour)})
				if !errors.As(err, &httpErr) || httpErr.Code != http.StatusForbidden || !strings.Contains(err.Error(), "iterations") {
					t.Errorf("expand err = %v, want 403 naming the iteration cap", err)
				}
			})
		}
	}
}
