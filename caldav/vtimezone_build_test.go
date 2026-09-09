package caldav

import (
	"archive/zip"
	"bytes"
	"go/build"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"

	_ "time/tzdata"
)

// shortModeZones keeps `go test -short` quick. The full run checks every zone
// the runtime knows, so no list of interesting zones has to be maintained by
// hand and none can silently go stale.
const shortModeZones = 40

// postHorizonYears is how far past the probe window the generated tail is
// compared against Go. An era published without a bound claims to run forever;
// this is where that claim is tested.
const postHorizonYears = 12

// baselineYearStride spaces the readings taken whether or not a zone changes.
const baselineYearStride = 5

// standingRuleFloorPercent is how much of the still-changing population must
// carry a standing rule. The shortfall is zones whose changeovers a yearly
// recurrence cannot express.
const standingRuleFloorPercent = 90

// zoneNames lists every zone in the database Go ships, which is the same data
// time.LoadLocation reads.
func zoneNames(t *testing.T) []string {
	t.Helper()

	archive, err := zip.OpenReader(filepath.Join(build.Default.GOROOT, "lib", "time", "zoneinfo.zip"))
	if err != nil {
		t.Skipf("cannot enumerate zones: %v", err)
	}
	defer archive.Close()

	names := make([]string, 0, len(archive.File))
	for _, file := range archive.File {
		if strings.HasSuffix(file.Name, "/") || strings.Contains(file.Name, ".") {
			continue
		}
		names = append(names, file.Name)
	}

	if !testing.Short() || len(names) <= shortModeZones {
		return names
	}

	stride := len(names) / shortModeZones
	sampled := make([]string, 0, shortModeZones+1)
	for i := 0; i < len(names); i += stride {
		sampled = append(sampled, names[i])
	}

	return sampled
}

func encodeCalendar(t *testing.T, component *ical.Component) []byte {
	t.Helper()

	calendar := ical.NewCalendar()
	calendar.Props.SetText(ical.PropProductID, "-//davkit//test//EN")
	calendar.Props.SetText(ical.PropVersion, "2.0")
	calendar.Children = append(calendar.Children, component)

	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(calendar); err != nil {
		t.Fatalf("encoding: %v", err)
	}

	return buf.Bytes()
}

// Every zone the runtime knows is generated, parsed back with this package's
// own resolver, and asked for the offset either side of every changeover the
// runtime reports, then well past the probe window where only a published
// open-ended rule can answer. Go is the oracle throughout, so no expected
// value is written down by hand and no zone has to be nominated as
// interesting.
func TestVTimezoneAgreesWithGo(t *testing.T) {
	for _, name := range zoneNames(t) {
		t.Run(name, func(t *testing.T) {
			location, err := time.LoadLocation(name)
			if err != nil {
				t.Skipf("cannot load: %v", err)
			}

			component, err := VTimezone(name)
			if err != nil {
				t.Fatalf("generating: %v", err)
			}

			checkStructure(t, component, name)
			checkDeterminism(t, component, name)

			resolver, err := newTZResolver(component)
			if err != nil {
				t.Fatalf("generated definition does not parse: %v", err)
			}

			for _, instant := range checkInstants(location, tailIsComplete(component, location)) {
				wall, want, ok := unambiguousReading(instant, location)
				if !ok {
					continue
				}

				got, err := resolver.offsetAt(wall)
				if err != nil {
					t.Fatalf("resolving %s: %v", wall.Format(time.RFC3339), err)
				}
				if got != want {
					t.Errorf("at %s: offset %v, want %v", wall.Format("2006-01-02T15:04:05"), got, want)
				}
			}
		})
	}
}

// checkInstants are two hours either side of every changeover, which is where
// an off-by-one rule shows itself. Readings past the horizon are added only
// for a zone whose tail is complete, since that is the whole of what the
// generator promises there.
func checkInstants(location *time.Location, includePostHorizon bool) []time.Time {
	transitions := zoneTransitions(location, vtzAnchorYear, vtzHorizonYear)

	instants := make([]time.Time, 0, len(transitions)*2+postHorizonYears*2)

	// Sample the window regardless of whether the zone ever changes. Without
	// this a zone with no changeover is asked nothing at all, which is 184 of
	// them — every fixed-offset zone, UTC included — so their emitted offset
	// would never be compared against Go.
	for year := vtzAnchorYear; year < vtzHorizonYear; year += baselineYearStride {
		instants = append(instants,
			time.Date(year, time.January, 15, 12, 0, 0, 0, time.UTC),
			time.Date(year, time.July, 15, 12, 0, 0, 0, time.UTC))
	}

	for i := range transitions {
		// Clear the changeover by more than it moves the clock. A zone that
		// puts its clock back two or three hours — Alaska in 1983, Rothera on
		// the day it was first occupied — repeats that many hours of wall
		// clock, and a reading inside the repeat has two right answers.
		margin := transitions[i].offsetTo - transitions[i].offsetFrom
		if margin < 0 {
			margin = -margin
		}
		margin += time.Hour

		instants = append(instants,
			transitions[i].at.Add(-margin),
			transitions[i].at.Add(margin))
	}

	if !includePostHorizon {
		return instants
	}

	// Past the horizon, sample around the changeovers Go reports rather than
	// mid-season. Mid-season readings only pin the two seasonal offsets, so a
	// published rule that puts the changeover in the wrong week — or the wrong
	// month — sails through them.
	for _, transition := range zoneTransitions(location, vtzHorizonYear, vtzHorizonYear+postHorizonYears) {
		margin := transition.offsetTo - transition.offsetFrom
		if margin < 0 {
			margin = -margin
		}
		margin += time.Hour

		instants = append(instants,
			transition.at.Add(-margin),
			transition.at.Add(margin))
	}

	return instants
}

// tailIsComplete reports whether the published open-ended rules account for
// every changeover in the zone's final probed year. When they do, the
// definition is exact for all later years and is held to that. When they do
// not — a zone whose changeovers follow something a yearly recurrence cannot
// express, such as Ramadan — the generator only promises the probe window, so
// there is nothing to assert beyond it. The distinction is derived from the
// zone, never from a list of names.
func tailIsComplete(component *ical.Component, location *time.Location) bool {
	published := 0
	for _, observance := range component.Children {
		rule := observance.Props.Get(ical.PropRecurrenceRule)
		if rule == nil {
			continue
		}
		if !strings.Contains(rule.Value, "COUNT=") && !strings.Contains(rule.Value, "UNTIL=") {
			published++
		}
	}

	expected := changeoversInFinalYear(zoneTransitions(location, vtzAnchorYear, vtzHorizonYear))

	return published > 0 && published == expected
}

// unambiguousReading converts an instant to the wall clock a person would read
// off a clock in that zone, and reports whether that reading identifies the
// instant uniquely.
//
// Around a changeover it does not. An hour skipped forward never happens, and
// an hour put back happens twice, so a wall clock inside those windows has
// either no answer or two. Both this package and Go pick one, and they are
// entitled to pick differently, so those readings say nothing about whether
// the generated definition is right. Round-tripping the wall clock back
// through the zone is what separates them: only an unambiguous reading returns
// the instant it came from.
func unambiguousReading(instant time.Time, location *time.Location) (wall time.Time, offset time.Duration, ok bool) {
	_, seconds := instant.In(location).Zone()
	offset = time.Duration(seconds) * time.Second
	wall = instant.Add(offset).UTC()

	roundTrip := time.Date(wall.Year(), wall.Month(), wall.Day(),
		wall.Hour(), wall.Minute(), wall.Second(), 0, location)

	return wall, offset, roundTrip.Equal(instant)
}

// checkStructure asserts what RFC 5545 requires of the component, for every
// zone rather than for one chosen example.
func checkStructure(t *testing.T, component *ical.Component, name string) {
	t.Helper()

	if component.Name != ical.CompTimezone {
		t.Fatalf("component is %q, want VTIMEZONE", component.Name)
	}

	id, err := component.Props.Text(ical.PropTimezoneID)
	if err != nil || id == "" {
		t.Fatalf("TZID is missing or empty: %v", err)
	}
	if id != name {
		t.Errorf("TZID is %q, want %q", id, name)
	}
	if len(component.Children) == 0 {
		t.Fatal("no observances")
	}

	for _, observance := range component.Children {
		if observance.Name != ical.CompTimezoneStandard && observance.Name != ical.CompTimezoneDaylight {
			t.Errorf("observance is %q, want STANDARD or DAYLIGHT", observance.Name)
		}
		for _, required := range []string{ical.PropDateTimeStart, "TZOFFSETFROM", "TZOFFSETTO"} {
			if len(observance.Props[required]) != 1 {
				t.Errorf("%s has %d %s properties, want exactly 1",
					observance.Name, len(observance.Props[required]), required)
			}
		}

		start := observance.Props.Get(ical.PropDateTimeStart)
		if start == nil {
			continue
		}
		// An onset is a wall clock in the offset being left, so it carries
		// neither a Z nor a TZID.
		if strings.HasSuffix(start.Value, "Z") || start.Params.Get(ical.ParamTimezoneID) != "" {
			t.Errorf("DTSTART %q must be floating local time", start.Value)
		}
	}
}

// The definition is embedded in stored objects, so identical input must give
// identical bytes or every ETag churns for no reason.
func checkDeterminism(t *testing.T, component *ical.Component, name string) {
	t.Helper()

	again, err := VTimezone(name)
	if err != nil {
		t.Fatalf("regenerating: %v", err)
	}
	if !bytes.Equal(encodeCalendar(t, component), encodeCalendar(t, again)) {
		t.Error("two calls produced different bytes")
	}

	// Two calls microseconds apart cannot see a window derived from the clock,
	// which is the drift the fixed bounds exist to prevent. Pin the bounds
	// themselves: every onset and every rule bound must sit inside them, so a
	// window that moved with the year would show up here on any run.
	for _, observance := range component.Children {
		start := observance.Props.Get(ical.PropDateTimeStart)
		if start == nil {
			continue
		}
		if year := start.Value[:4]; year < strconv.Itoa(vtzAnchorYear) || year >= strconv.Itoa(vtzHorizonYear) {
			t.Errorf("DTSTART %q falls outside the fixed window", start.Value)
		}

		rule := observance.Props.Get(ical.PropRecurrenceRule)
		if rule == nil {
			continue
		}
		_, bound, found := strings.Cut(rule.Value, "UNTIL=")
		if !found {
			continue
		}
		if year := bound[:4]; year >= strconv.Itoa(vtzHorizonYear) {
			t.Errorf("UNTIL %q falls outside the fixed window", bound)
		}
	}
}

// The per-zone sweep only checks a zone past the horizon when that zone
// published a standing rule, so a generator that published none at all would
// switch the check off rather than fail it, leaving every zone stuck on its
// last changeover. Nothing per-zone can see that; this can.
//
// The expectation comes from the zones themselves: a zone still changing at
// the end of the probe should carry a rule saying so. A few cannot, because
// their changeovers follow something a yearly recurrence will not express, so
// this asserts the two counts stay close rather than equal — close enough to
// catch a generator that stopped publishing rules, loose enough not to encode
// which zones are awkward this year.
func TestVTimezonePublishesStandingRules(t *testing.T) {
	stillChanging, withTail := 0, 0

	for _, name := range zoneNames(t) {
		location, err := time.LoadLocation(name)
		if err != nil {
			continue
		}
		if changeoversInFinalYear(zoneTransitions(location, vtzAnchorYear, vtzHorizonYear)) > 0 {
			stillChanging++
		}

		component, err := VTimezone(name)
		if err != nil {
			t.Fatalf("generating %s: %v", name, err)
		}
		for _, observance := range component.Children {
			rule := observance.Props.Get(ical.PropRecurrenceRule)
			if rule != nil && !strings.Contains(rule.Value, "UNTIL=") {
				withTail++
				break
			}
		}
	}

	if floor := stillChanging * standingRuleFloorPercent / 100; withTail < floor {
		t.Fatalf("%d zones still change at the end of the window but only %d publish a standing rule"+
			" (want at least %d); a zone that publishes none is stuck on its last changeover forever",
			stillChanging, withTail, floor)
	}
}

// A name that is not an interchangeable zone identifier must be refused rather
// than yielding a definition nobody can resolve. The empty string and "Local"
// both load in Go, and both produce output that is either invalid or dependent
// on the machine that generated it.
func TestVTimezoneRejectsUnusableNames(t *testing.T) {
	for _, name := range []string{"", "Local", "Mars/Olympus_Mons"} {
		t.Run("name="+name, func(t *testing.T) {
			if _, err := VTimezone(name); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
