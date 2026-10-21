package caldav

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

func rangedChildren(t *testing.T, bodies ...string) []*ical.Component {
	t.Helper()

	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n"
	for _, body := range bodies {
		ics += "BEGIN:VEVENT\r\nDTSTAMP:20260801T000000Z\r\n" + body + "END:VEVENT\r\n"
	}
	ics += "END:VCALENDAR\r\n"
	cal, err := ical.NewDecoder(strings.NewReader(ics)).Decode()
	if err != nil {
		t.Fatalf("decoding fixture: %v", err)
	}
	return cal.Children
}

func instant(t *testing.T, value string) time.Time {
	t.Helper()

	parsed, err := time.Parse("20060102T150405Z", value)
	if err != nil {
		t.Fatalf("parsing %q: %v", value, err)
	}
	return parsed
}

func TestOverrideIndexOrdersRangesAndBoundsEachBackwardShift(t *testing.T) {
	children := rangedChildren(t,
		"UID:a\r\nDTSTART:20260801T090000Z\r\nDTEND:20260801T100000Z\r\nRRULE:FREQ=DAILY\r\n",
		"UID:a\r\nSUMMARY:Late\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260810T090000Z\r\nDTSTART:20260810T070000Z\r\nDTEND:20260810T080000Z\r\n",
		"UID:a\r\nSUMMARY:Exact\r\nRECURRENCE-ID:20260805T090000Z\r\nDTSTART:20260805T150000Z\r\nDTEND:20260805T160000Z\r\n",
		"UID:a\r\nSUMMARY:Early\r\nRECURRENCE-ID;RANGE=thisandfuture:20260803T090000Z\r\nDTSTART:20260803T040000Z\r\nDTEND:20260803T050000Z\r\n",
		"UID:a\r\nSUMMARY:Forward\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260807T090000Z\r\nDTSTART:20260807T120000Z\r\nDTEND:20260807T130000Z\r\n",
		"UID:b\r\nSUMMARY:Other\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260802T090000Z\r\nDTSTART:20260802T100000Z\r\nDTEND:20260802T110000Z\r\n",
	)
	indexes, err := buildOverrideIndexes(children, time.UTC)
	if err != nil {
		t.Fatalf("building: %v", err)
	}

	idx := indexes["a"]
	order := make([]string, 0, len(idx.ranged))
	for _, r := range idx.ranged {
		order = append(order, propValue(r.comp, ical.PropSummary))
	}
	if got, want := strings.Join(order, ","), "Early,Forward,Late"; got != want {
		t.Errorf("ranged order = %s, want %s", got, want)
	}
	if got := len(idx.exact); got != 4 {
		t.Errorf("exact overrides = %d, want 4, ranged anchors included", got)
	}
	if got, want := indexes["b"].horizon(instant(t, "20260901T000000Z")), instant(t, "20260901T000000Z"); !got.Equal(want) {
		t.Errorf("horizon of a forward-only UID = %v, want %v", got, want)
	}
	// Early moves back five hours but Forward takes over on 7 August, so past
	// that only Late's two hours count.
	horizons := map[string]string{
		"20260901T000000Z": "20260901T020000Z",
		"20260803T120000Z": "20260803T170000Z",
		"20260807T060000Z": "20260807T090000Z",
	}
	for end, want := range horizons {
		if got := idx.horizon(instant(t, end)); !got.Equal(instant(t, want)) {
			t.Errorf("horizon(%s) = %v, want %s", end, got, want)
		}
	}
	if got := idx.horizon(time.Time{}); !got.IsZero() {
		t.Errorf("horizon of an open window = %v, want unbounded", got)
	}
}

func TestSeriesWalkResolvesEachOccurrenceToItsGoverningOverride(t *testing.T) {
	children := rangedChildren(t,
		"UID:a\r\nSUMMARY:Master\r\nDTSTART:20260801T090000Z\r\nDTEND:20260801T100000Z\r\nRRULE:FREQ=DAILY\r\n",
		"UID:a\r\nSUMMARY:B\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260807T090000Z\r\nDTSTART:20260807T070000Z\r\nDTEND:20260807T080000Z\r\n",
		"UID:a\r\nSUMMARY:A\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z\r\nDTSTART:20260803T100000Z\r\nDTEND:20260803T120000Z\r\n",
		"UID:a\r\nSUMMARY:X\r\nRECURRENCE-ID:20260805T090000Z\r\nDTSTART:20260805T150000Z\r\nDTEND:20260805T160000Z\r\n",
	)
	indexes, err := buildOverrideIndexes(children, time.UTC)
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	master := children[0]
	extent, err := extentOf(master, time.UTC)
	if err != nil {
		t.Fatalf("extent: %v", err)
	}
	walk := newSeriesWalk(master, &extent, indexes["a"])

	tests := []struct {
		original  string
		kind      instanceKind
		template  string
		effective string
	}{
		{"20260802T090000Z", fromMaster, "Master", "20260802T090000Z"},
		{"20260803T090000Z", fromExact, "A", ""},
		{"20260804T090000Z", fromRange, "A", "20260804T100000Z"},
		{"20260805T090000Z", fromExact, "X", ""},
		{"20260806T090000Z", fromRange, "A", "20260806T100000Z"},
		{"20260807T090000Z", fromExact, "B", ""},
		{"20260808T090000Z", fromRange, "B", "20260808T070000Z"},
	}
	for _, tt := range tests {
		inst, resolveErr := walk.resolve(occurrence{start: instant(t, tt.original)})
		if resolveErr != nil {
			t.Fatalf("%s: resolve: %v", tt.original, resolveErr)
		}
		if inst.kind != tt.kind {
			t.Errorf("%s: kind = %d, want %d", tt.original, inst.kind, tt.kind)
		}
		if got := propValue(inst.template, ical.PropSummary); got != tt.template {
			t.Errorf("%s: template = %s, want %s", tt.original, got, tt.template)
		}
		if tt.effective != "" && !inst.effective.start.Equal(instant(t, tt.effective)) {
			t.Errorf("%s: effective start = %v, want %s", tt.original, inst.effective.start, tt.effective)
		}
	}

	inst, err := walk.resolve(occurrence{start: instant(t, "20260809T090000Z")})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	overlaps, err := inst.overlaps(instant(t, "20260809T073000Z"), instant(t, "20260809T074500Z"))
	if err != nil || !overlaps {
		t.Errorf("overlap at the ranged time = %v, %v; want true", overlaps, err)
	}
}

func TestBuildingTheIndexRefusesAMalformedRange(t *testing.T) {
	tests := map[string]string{
		"no DTSTART":         "UID:a\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z\r\nDTEND:20260803T100000Z\r\n",
		"unreadable DTSTART": "UID:a\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z\r\nDTSTART:soon\r\n",
		"unreadable DTEND":   "UID:a\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z\r\nDTSTART:20260803T090000Z\r\nDTEND:later\r\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := buildOverrideIndexes(rangedChildren(t, body), time.UTC)
			if !errors.Is(err, errMalformedRangedOverride) {
				t.Errorf("err = %v, want errMalformedRangedOverride", err)
			}
		})
	}

	// The same component without a range names one instance and needs no
	// move, so it does not stop the index being built.
	exact := "UID:a\r\nRECURRENCE-ID:20260803T090000Z\r\nDTEND:20260803T100000Z\r\n"
	if _, err := buildOverrideIndexes(rangedChildren(t, exact), time.UTC); err != nil {
		t.Errorf("exact override: err = %v, want nil", err)
	}
}

func TestOverrideInstanceWritesAFreshRecurrenceID(t *testing.T) {
	template := rangedChildren(t,
		"UID:a\r\nSUMMARY:Ranged\r\nRECURRENCE-ID;TZID=America/Los_Angeles;RANGE=THISANDFUTURE:20260803T090000\r\n"+
			"DTSTART:20260803T110000Z\r\nDTEND:20260803T130000Z\r\nRRULE:FREQ=DAILY\r\nRDATE:20260820T110000Z\r\nEXDATE:20260821T110000Z\r\n",
	)[0]
	const twoDays = 48 * 60 * 60

	instance, err := overrideInstance(template, twoDays, instant(t, "20260805T160000Z"), false)
	if err != nil {
		t.Fatalf("overrideInstance: %v", err)
	}

	want := map[string]string{
		ical.PropDateTimeStart: "20260805T110000Z",
		ical.PropDateTimeEnd:   "20260805T130000Z",
		ical.PropRecurrenceID:  "20260805T160000Z",
		ical.PropSummary:       "Ranged",
	}
	for name, value := range want {
		if got := propValue(instance, name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	for _, name := range []string{ical.PropRecurrenceRule, ical.PropRecurrenceDates, ical.PropExceptionDates} {
		if instance.Props.Get(name) != nil {
			t.Errorf("%s survived on the instance", name)
		}
	}
	rid := instance.Props.Get(ical.PropRecurrenceID)
	if len(rid.Params) != 0 {
		t.Errorf("RECURRENCE-ID params = %v, want none", rid.Params)
	}
	if got := propValue(template, ical.PropDateTimeStart); got != "20260803T110000Z" {
		t.Errorf("template DTSTART = %q, want it left as it was", got)
	}
}

func TestOverrideInstanceKeepsEachValueType(t *testing.T) {
	template := rangedChildren(t,
		"UID:a\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z\r\nDTSTART;VALUE=DATE:20260803\r\nDURATION:P1D\r\n",
	)[0]

	const oneDay = 24 * 60 * 60

	instance, err := overrideInstance(template, oneDay, instant(t, "20260804T090000Z"), false)
	if err != nil {
		t.Fatalf("overrideInstance: %v", err)
	}

	start := instance.Props.Get(ical.PropDateTimeStart)
	if !isDate(start) || start.Value != "20260804" {
		t.Errorf("DTSTART = %s %q, want DATE 20260804", start.ValueType(), start.Value)
	}
	if got := propValue(instance, ical.PropDuration); got != "P1D" {
		t.Errorf("DURATION = %q, want it kept as spelt", got)
	}
	if rid := instance.Props.Get(ical.PropRecurrenceID); isDate(rid) || rid.Value != "20260804T090000Z" {
		t.Errorf("RECURRENCE-ID = %s %q, want DATE-TIME 20260804T090000Z", rid.ValueType(), rid.Value)
	}

	dated, err := overrideInstance(template, oneDay, instant(t, "20260804T000000Z"), true)
	if err != nil {
		t.Fatalf("overrideInstance: %v", err)
	}
	if rid := dated.Props.Get(ical.PropRecurrenceID); !isDate(rid) || rid.Value != "20260804" {
		t.Errorf("RECURRENCE-ID = %s %q, want DATE 20260804", rid.ValueType(), rid.Value)
	}
}

func TestShiftInstantMovesBySecondsWithinTheWritableYears(t *testing.T) {
	const year = 365 * 24 * 60 * 60
	tests := []struct {
		name    string
		from    string
		seconds int64
		want    string
	}{
		{"millennia ahead", "10010101T090000Z", 7999*year + 1940*24*60*60, "90000101T090000Z"},
		{"back to the first instant", "00010101T010000Z", -60 * 60, "00010101T000000Z"},
		{"to the last instant", "99991231T235958Z", 1, "99991231T235959Z"},
		{"past the last instant", "99991231T235959Z", 1, ""},
		{"before the first instant", "00010101T000000Z", -1, ""},
		{"from a time no year can write", "", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from := time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
			if tt.from != "" {
				from = instant(t, tt.from)
			}
			got, err := shiftInstant(from, tt.seconds)
			if tt.want == "" {
				if !errors.Is(err, errInstantOutOfRange) {
					t.Errorf("err = %v, want errInstantOutOfRange", err)
				}
				return
			}
			if err != nil || !got.Equal(instant(t, tt.want)) {
				t.Errorf("shiftInstant = %v, %v; want %s", got, err, tt.want)
			}
		})
	}
}

func TestHorizonIgnoresRangesThatCannotReachTheWindow(t *testing.T) {
	tests := map[string]struct {
		bodies []string
		end    string
	}{
		"a range superseded at its own anchor": {
			bodies: []string{
				"UID:a\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20261201T000000Z\r\nDTSTART:20251201T000000Z\r\n",
				"UID:a\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20261201T000000Z\r\nDTSTART:20261201T000000Z\r\n",
			},
			end: "20260802T000031Z",
		},
		"a range anchored beyond what its move can bring back": {
			bodies: []string{
				"UID:a\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260810T000000Z\r\nDTSTART:20260809T230000Z\r\n",
			},
			end: "20260801T000000Z",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			indexes, err := buildOverrideIndexes(rangedChildren(t, tt.bodies...), time.UTC)
			if err != nil {
				t.Fatalf("building: %v", err)
			}
			end := instant(t, tt.end)
			if got := indexes["a"].horizon(end); !got.Equal(end) {
				t.Errorf("horizon = %v, want the window end %v", got, end)
			}
		})
	}
}
