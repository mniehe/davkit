package caldav

import (
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

// RFC 5545 section 3.6.5 requires a bounded observance to be terminated with
// UNTIL, expressed as a UTC instant, while the onsets it bounds are wall
// clocks in the offset being left. Every conforming server emits that shape,
// so davkit has to read it correctly whoever produced it.
//
// Berlin moved its autumn changeover from September to October in 1996, which
// makes the earlier run a bounded era. Its onsets are 03:00 local under +0200,
// so the UTC instant naming the last of them is 01:00Z: earlier in the day
// than the wall clock, which is what a naive comparison gets wrong. West of
// Greenwich the instant lands later in the day instead, so the same comparison
// happens to come out right, which is why this survived.
const berlinRFCStyle = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//davkit//test//EN
BEGIN:VTIMEZONE
TZID:Europe/Berlin
BEGIN:DAYLIGHT
DTSTART:19810329T020000
TZOFFSETFROM:+0100
TZOFFSETTO:+0200
RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU
END:DAYLIGHT
BEGIN:STANDARD
DTSTART:19810927T030000
TZOFFSETFROM:+0200
TZOFFSETTO:+0100
RRULE:FREQ=YEARLY;BYMONTH=9;BYDAY=-1SU;UNTIL=19950924T010000Z
END:STANDARD
BEGIN:STANDARD
DTSTART:19961027T030000
TZOFFSETFROM:+0200
TZOFFSETTO:+0100
RRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU
END:STANDARD
END:VTIMEZONE
END:VCALENDAR
`

// New York from the RFC's own example, to prove the fix does not disturb the
// case that already worked.
const newYorkRFCStyle = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//davkit//test//EN
BEGIN:VTIMEZONE
TZID:America/New_York
BEGIN:DAYLIGHT
DTSTART:19870405T020000
TZOFFSETFROM:-0500
TZOFFSETTO:-0400
RRULE:FREQ=YEARLY;BYMONTH=4;BYDAY=1SU;UNTIL=20060402T070000Z
END:DAYLIGHT
BEGIN:STANDARD
DTSTART:19871025T020000
TZOFFSETFROM:-0400
TZOFFSETTO:-0500
RRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU;UNTIL=20061029T060000Z
END:STANDARD
BEGIN:DAYLIGHT
DTSTART:20070311T020000
TZOFFSETFROM:-0500
TZOFFSETTO:-0400
RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU
END:DAYLIGHT
BEGIN:STANDARD
DTSTART:20071104T020000
TZOFFSETFROM:-0400
TZOFFSETTO:-0500
RRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU
END:STANDARD
END:VTIMEZONE
END:VCALENDAR
`

// The same Berlin definition with a floating UNTIL. RFC 5545 §3.3.10 requires
// this form when DTSTART is local time, which a timezone onset always is, so
// both shapes arrive from real clients even though §3.6.5's own example uses
// the UTC form. A floating bound already sits in the frame its onsets use and
// must be left exactly alone.
var berlinFloatingUntil = strings.Replace(berlinRFCStyle,
	"UNTIL=19950924T010000Z", "UNTIL=19950924T030000", 1)

func resolverFor(t *testing.T, ics string) *tzResolver {
	t.Helper()

	calendar, err := ical.NewDecoder(strings.NewReader(strings.ReplaceAll(ics, "\n", "\r\n"))).Decode()
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	resolver, err := newTZResolver(calendar.Children[0])
	if err != nil {
		t.Fatalf("parsing VTIMEZONE: %v", err)
	}

	return resolver
}

// A floating bound is already in the onsets' frame, so shifting it would move
// the era's end by the offset and drop its final year all over again — the
// very failure the shift exists to prevent, inflicted on the other shape.
func TestResolverReadsFloatingUntil(t *testing.T) {
	location, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("cannot load Europe/Berlin: %v", err)
	}
	resolver := resolverFor(t, berlinFloatingUntil)

	for year := 1990; year <= 2000; year++ {
		for month := time.January; month <= time.December; month++ {
			wall := time.Date(year, month, 15, 12, 0, 0, 0, time.UTC)

			got, err := resolver.offsetAt(wall)
			if err != nil {
				t.Fatalf("resolving %s: %v", wall.Format(time.RFC3339), err)
			}

			_, seconds := time.Date(year, month, 15, 12, 0, 0, 0, location).Zone()
			if want := time.Duration(seconds) * time.Second; got != want {
				t.Errorf("at %s: offset %v, want %v", wall.Format("2006-01"), got, want)
			}
		}
	}
}

func TestResolverReadsUTCUntil(t *testing.T) {
	cases := map[string]string{
		"Europe/Berlin":    berlinRFCStyle,
		"America/New_York": newYorkRFCStyle,
	}

	for zone, ics := range cases {
		t.Run(zone, func(t *testing.T) {
			location, err := time.LoadLocation(zone)
			if err != nil {
				t.Skipf("cannot load %s: %v", zone, err)
			}
			resolver := resolverFor(t, ics)

			// Every month across the years either side of each bounded era's
			// end, which is the only place the bound can be misread.
			for year := 1990; year <= 2010; year++ {
				for month := time.January; month <= time.December; month++ {
					wall := time.Date(year, month, 15, 12, 0, 0, 0, time.UTC)

					got, err := resolver.offsetAt(wall)
					if err != nil {
						t.Fatalf("resolving %s: %v", wall.Format(time.RFC3339), err)
					}

					_, seconds := time.Date(year, month, 15, 12, 0, 0, 0, location).Zone()
					if want := time.Duration(seconds) * time.Second; got != want {
						t.Errorf("at %s: offset %v, want %v", wall.Format("2006-01"), got, want)
					}
				}
			}
		})
	}
}

// Two properties hold for every bound, whatever shape it arrived in: it never
// moves earlier than the value as written, because that is what would drop the
// onset it names; and it stays a representable four-digit year, because the
// conventional "forever" bound is 99991231T235959Z and nudging that forward
// yields something no recurrence parser will read.
func TestResolverBoundStaysUsable(t *testing.T) {
	bounds := []string{
		"99991231T235959Z", "00010101T000000Z",
		"19950924T010000Z", "19950924T030000",
		"20060402T070000Z",
	}
	offsets := []time.Duration{14 * time.Hour, time.Hour, 0, -time.Hour, -12 * time.Hour}

	for _, until := range bounds {
		for _, offset := range offsets {
			t.Run(until+"/"+offset.String(), func(t *testing.T) {
				observance := ical.NewComponent(ical.CompTimezoneDaylight)
				observance.Props.Set(rawProp(ical.PropRecurrenceRule,
					"FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU;UNTIL="+until))

				got := shiftUntilToOnsetFrame(observance, offset).
					Props.Get(ical.PropRecurrenceRule).Value
				_, bound, found := strings.Cut(got, "UNTIL=")
				if !found {
					t.Fatalf("the bound disappeared: %q", got)
				}

				parsed, err := time.ParseInLocation(localDateTimeLayout, strings.TrimSuffix(bound, "Z"), time.UTC)
				if err != nil {
					t.Fatalf("bound %q is not a readable DATE-TIME: %v", bound, err)
				}
				if parsed.Year() < minCalendarYear || parsed.Year() > maxCalendarYear {
					t.Errorf("bound %q left the representable range", bound)
				}

				written, err := time.ParseInLocation(localDateTimeLayout, strings.TrimSuffix(until, "Z"), time.UTC)
				if err != nil {
					t.Fatal(err)
				}
				if parsed.Before(written) {
					t.Errorf("bound moved earlier, from %s to %s", until, bound)
				}
			})
		}
	}
}
