package caldav

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

var dateFormat = "20060102T150405Z"

const partstatICS = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Test//EN
BEGIN:VEVENT
UID:u1
DTSTAMP:20260101T000000Z
DTSTART:20260101T100000Z
ATTENDEE;PARTSTAT=ACCEPTED:mailto:cyrus@example.com
END:VEVENT
END:VCALENDAR`

func attendeeFilter(params ...paramFilter) *compFilter {
	return &compFilter{
		Name: "VCALENDAR",
		Comps: []compFilter{{
			Name:  "VEVENT",
			Props: []propFilter{{Name: "ATTENDEE", ParamFilter: params}},
		}},
	}
}

// RFC 4791 §9.7.3: a param-filter constrains the parameters of the matched
// property. Mirrors carddav's TestParamFilterIsApplied.
func TestParamFilterIsApplied(t *testing.T) {
	for _, tc := range []struct {
		name  string
		param paramFilter
		want  bool
	}{
		{"matching parameter value", paramFilter{Name: "PARTSTAT", TextMatch: &textMatch{Text: "accepted"}}, true},
		{"non-matching parameter value", paramFilter{Name: "PARTSTAT", TextMatch: &textMatch{Text: "declined"}}, false},
		{"parameter present", paramFilter{Name: "PARTSTAT"}, true},
		{"parameter absent", paramFilter{Name: "ROLE"}, false},
		{"is-not-defined on an absent parameter", paramFilter{Name: "ROLE", IsNotDefined: true}, true},
		{"is-not-defined on a present parameter", paramFilter{Name: "PARTSTAT", IsNotDefined: true}, false},
		{"negated text-match", paramFilter{Name: "PARTSTAT", TextMatch: &textMatch{Text: "declined", NegateCondition: true}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			co := &calendarObject{Path: "/u/c/e.ics", Data: parseCalendar(t, partstatICS)}
			matched, err := matchObject(attendeeFilter(tc.param), co)
			if err != nil {
				t.Fatalf("Match: %v", err)
			}
			if matched != tc.want {
				t.Errorf("matched = %v, want %v", matched, tc.want)
			}
		})
	}
}

// caldav reads a parameter with ical.Params.Get, which returns "" both for an
// absent parameter and for one present with an empty value, so a present-but-
// empty parameter answers is-not-defined. carddav's two-value map lookup tells
// the two apart and reports such a parameter as defined.
func TestParamFilterTreatsAnEmptyValueAsUndefined(t *testing.T) {
	object := func(t *testing.T) *calendarObject {
		t.Helper()
		cal := parseCalendar(t, partstatICS)
		event := cal.Children[0]
		event.Props.Get("ATTENDEE").Params["ROLE"] = []string{""}
		return &calendarObject{Path: "/u/c/e.ics", Data: cal}
	}

	matched, err := matchObject(attendeeFilter(paramFilter{Name: "ROLE"}), object(t))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if matched {
		t.Error("a present-but-empty parameter satisfied a param-filter requiring it to be defined")
	}

	matched, err = matchObject(attendeeFilter(paramFilter{Name: "ROLE", IsNotDefined: true}), object(t))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !matched {
		t.Error("a present-but-empty parameter did not satisfy is-not-defined")
	}
}

func toDate(t *testing.T, date string) time.Time {
	res, err := time.ParseInLocation(dateFormat, date, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Test data taken from https://datatracker.ietf.org/doc/html/rfc4791#appendix-B
// TODO add missing data
func TestFilter(t *testing.T) {
	newCO := func(str string) calendarObject {
		cal, err := ical.NewDecoder(strings.NewReader(str)).Decode()
		if err != nil {
			t.Fatal(err)
		}
		return calendarObject{
			Data: cal,
		}
	}

	event1 := newCO(`BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Example Corp.//CalDAV Client//EN
BEGIN:VTIMEZONE
LAST-MODIFIED:20040110T032845Z
TZID:US/Eastern
BEGIN:DAYLIGHT
DTSTART:20000404T020000
RRULE:FREQ=YEARLY;BYDAY=1SU;BYMONTH=4
TZNAME:EDT
TZOFFSETFROM:-0500
TZOFFSETTO:-0400
END:DAYLIGHT
BEGIN:STANDARD
DTSTART:20001026T020000
RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=10
TZNAME:EST
TZOFFSETFROM:-0400
TZOFFSETTO:-0500
END:STANDARD
END:VTIMEZONE
BEGIN:VEVENT
DTSTAMP:20060206T001102Z
DTSTART;TZID=US/Eastern:20060102T100000
DURATION:PT1H
SUMMARY:Event #1
Description:Go Steelers!
UID:74855313FA803DA593CD579A@example.com
END:VEVENT
END:VCALENDAR`)

	event2 := newCO(`BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Example Corp.//CalDAV Client//EN
BEGIN:VTIMEZONE
LAST-MODIFIED:20040110T032845Z
TZID:US/Eastern
BEGIN:DAYLIGHT
DTSTART:20000404T020000
RRULE:FREQ=YEARLY;BYDAY=1SU;BYMONTH=4
TZNAME:EDT
TZOFFSETFROM:-0500
TZOFFSETTO:-0400
END:DAYLIGHT
BEGIN:STANDARD
DTSTART:20001026T020000
RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=10
TZNAME:EST
TZOFFSETFROM:-0400
TZOFFSETTO:-0500
END:STANDARD
END:VTIMEZONE
BEGIN:VEVENT
DTSTAMP:20060206T001121Z
DTSTART;TZID=US/Eastern:20060102T120000
DURATION:PT1H
RRULE:FREQ=DAILY;COUNT=5
SUMMARY:Event #2
UID:00959BC664CA650E933C892C@example.com
END:VEVENT
BEGIN:VEVENT
DTSTAMP:20060206T001121Z
DTSTART;TZID=US/Eastern:20060104T140000
DURATION:PT1H
RECURRENCE-ID;TZID=US/Eastern:20060104T120000
SUMMARY:Event #2 bis
UID:00959BC664CA650E933C892C@example.com
END:VEVENT
BEGIN:VEVENT
DTSTAMP:20060206T001121Z
DTSTART;TZID=US/Eastern:20060106T140000
DURATION:PT1H
RECURRENCE-ID;TZID=US/Eastern:20060106T120000
SUMMARY:Event #2 bis bis
UID:00959BC664CA650E933C892C@example.com
END:VEVENT
END:VCALENDAR`)

	event3 := newCO(`BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Example Corp.//CalDAV Client//EN
BEGIN:VTIMEZONE
LAST-MODIFIED:20040110T032845Z
TZID:US/Eastern
BEGIN:DAYLIGHT
DTSTART:20000404T020000
RRULE:FREQ=YEARLY;BYDAY=1SU;BYMONTH=4
TZNAME:EDT
TZOFFSETFROM:-0500
TZOFFSETTO:-0400
END:DAYLIGHT
BEGIN:STANDARD
DTSTART:20001026T020000
RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=10
TZNAME:EST
TZOFFSETFROM:-0400
TZOFFSETTO:-0500
END:STANDARD
END:VTIMEZONE
BEGIN:VEVENT
ATTENDEE;PARTSTAT=ACCEPTED;ROLE=CHAIR:mailto:cyrus@example.com
ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:lisa@example.com
DTSTAMP:20060206T001220Z
DTSTART;TZID=US/Eastern:20060104T100000
DURATION:PT1H
LAST-MODIFIED:20060206T001330Z
ORGANIZER:mailto:cyrus@example.com
SEQUENCE:1
STATUS:TENTATIVE
SUMMARY:Event #3
UID:DC6C50A017428C5216A2F1CD@example.com
END:VEVENT
END:VCALENDAR`)

	todo1 := newCO(`BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Example Corp.//CalDAV Client//EN
BEGIN:VTODO
DTSTAMP:20060205T235335Z
DUE;VALUE=DATE:20060104
STATUS:NEEDS-ACTION
SUMMARY:Task #1
UID:DDDEEB7915FA61233B861457@example.com
BEGIN:VALARM
ACTION:AUDIO
TRIGGER;RELATED=START:-PT10M
END:VALARM
END:VTODO
END:VCALENDAR`)

	for _, tc := range []struct {
		name  string
		query *calendarQuery
		addrs []calendarObject
		want  []calendarObject
		err   error
	}{
		{
			name:  "nil-query",
			query: nil,
			addrs: []calendarObject{event1, event2, event3, todo1},
			want:  []calendarObject{event1, event2, event3, todo1},
		},
		{
			// https://datatracker.ietf.org/doc/html/rfc4791#section-7.8.8
			name: "events only",
			query: &calendarQuery{
				CompFilter: compFilter{
					Name: "VCALENDAR",
					Comps: []compFilter{
						{
							Name: "VEVENT",
						},
					},
				},
			},
			addrs: []calendarObject{event1, event2, event3, todo1},
			want:  []calendarObject{event1, event2, event3},
		},
		{
			// https://datatracker.ietf.org/doc/html/rfc4791#section-7.8.1
			name: "events in time range",
			query: &calendarQuery{
				CompFilter: compFilter{
					Name: "VCALENDAR",
					Comps: []compFilter{
						{
							Name:  "VEVENT",
							Start: toDate(t, "20060104T000000Z"),
							End:   toDate(t, "20060105T000000Z"),
						},
					},
				},
			},
			addrs: []calendarObject{event1, event2, event3, todo1},
			want:  []calendarObject{event2, event3},
		},
		{
			// https://datatracker.ietf.org/doc/html/rfc4791#section-7.8.1
			name: "events in open time range (no end date)",
			query: &calendarQuery{
				CompFilter: compFilter{
					Name: "VCALENDAR",
					Comps: []compFilter{
						{
							Name:  "VEVENT",
							Start: toDate(t, "20060104T000000Z"),
						},
					},
				},
			},
			addrs: []calendarObject{event1, event2, event3, todo1},
			want:  []calendarObject{event2, event3},
		},
		{
			// https://datatracker.ietf.org/doc/html/rfc4791#section-7.8.6
			name: "events by UID",
			query: &calendarQuery{
				CompFilter: compFilter{
					Name: "VCALENDAR",
					Comps: []compFilter{
						{
							Name: "VEVENT",
							Props: []propFilter{{
								Name: "UID",
								TextMatch: &textMatch{
									Text: "DC6C50A017428C5216A2F1CD@example.com",
								},
							}},
						},
					},
				},
			},
			addrs: []calendarObject{event1, event2, event3, todo1},
			want:  []calendarObject{event3},
		},
		{
			// https://datatracker.ietf.org/doc/html/rfc4791#section-7.8.6
			name: "events by description substring",
			query: &calendarQuery{
				CompFilter: compFilter{
					Name: "VCALENDAR",
					Comps: []compFilter{
						{
							Name: "VEVENT",
							Props: []propFilter{{
								Name: "Description",
								TextMatch: &textMatch{
									Text: "Steelers",
								},
							}},
						},
					},
				},
			},
			addrs: []calendarObject{event1, event2, event3, todo1},
			want:  []calendarObject{event1},
		},
		{
			// Query a time range that only returns a result if recurrence is properly evaluated.
			name: "recurring events in time range",
			query: &calendarQuery{
				CompFilter: compFilter{
					Name: "VCALENDAR",
					Comps: []compFilter{
						{
							Name:  "VEVENT",
							Start: toDate(t, "20060103T000000Z"),
							End:   toDate(t, "20060104T000000Z"),
						},
					},
				},
			},
			addrs: []calendarObject{event1, event2, event3, todo1},
			want:  []calendarObject{event2},
		},
		// TODO add more examples
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := filterObjects(tc.query, tc.addrs)
			switch {
			case err != nil && tc.err == nil:
				t.Fatalf("unexpected error: %+v", err)
			case err != nil && tc.err != nil:
				if got, want := err.Error(), tc.err.Error(); got != want {
					t.Fatalf("invalid error:\ngot= %q\nwant=%q", got, want)
				}
			case err == nil && tc.err != nil:
				t.Fatalf("expected an error:\ngot= %+v\nwant=%+v", err, tc.err)
			case err == nil && tc.err == nil:
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("invalid filter values:\ngot= %+v\nwant=%+v", got, tc.want)
				}
			}
		})
	}
}

// A series whose master and ranged override both fail the property filters
// can match nowhere, so an open window answers that rather than walking an
// endless rule to the iteration cap.
func TestOpenWindowOverARangedSeriesStopsOnceEveryTemplateFails(t *testing.T) {
	const ics = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Test//EN
BEGIN:VEVENT
UID:u1
DTSTAMP:20260101T000000Z
SUMMARY:Master
DTSTART:20260801T090000Z
DTEND:20260801T100000Z
RRULE:FREQ=DAILY
END:VEVENT
BEGIN:VEVENT
UID:u1
DTSTAMP:20260101T000000Z
SUMMARY:Ranged
RECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z
DTSTART:20260803T110000Z
DTEND:20260803T120000Z
END:VEVENT
END:VCALENDAR`
	from := toDate(t, "20260901T000000Z")
	summary := func(text string) propFilter {
		return propFilter{Name: "SUMMARY", TextMatch: &textMatch{Text: text}}
	}
	tests := []struct {
		name    string
		props   []propFilter
		want    bool
		wantErr string
	}{
		{"neither template matches", []propFilter{summary("Elsewhere")}, false, ""},
		{"the range's template matches", []propFilter{summary("Ranged")}, true, ""},
		// A time-range on a property reads each instance's own times, so no
		// verdict carries from one instance to the next: the walk tests every
		// one and an endless rule reaches the cap, which is reported.
		{"a property time-range keeps the walk per instance", []propFilter{
			summary("Elsewhere"),
			{Name: "DTSTART", Start: from},
		}, false, "exceeded"},
		// A time-range on SUMMARY itself asks for a date in a text value, which
		// fails on the first instance tested, before any walk could run long.
		{"a time-range on SUMMARY fails as unreadable", []propFilter{
			{Name: "SUMMARY", Start: from, TextMatch: &textMatch{Text: "Elsewhere"}},
		}, false, "cannot process"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := &compFilter{Name: "VCALENDAR", Comps: []compFilter{{Name: "VEVENT", Start: from, Props: tt.props}}}
			co := &calendarObject{Path: "/u/c/e.ics", Data: parseCalendar(t, ics)}
			matched, err := matchObject(filter, co)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Match: %v", err)
			}
			if matched != tt.want {
				t.Errorf("matched = %v, want %v", matched, tt.want)
			}
		})
	}
}

func TestTimeDependentFiltersAreThoseReadingInstanceTimes(t *testing.T) {
	at := toDate(t, "20260901T000000Z")
	tests := []struct {
		name   string
		filter compFilter
		want   bool
	}{
		{"text only", compFilter{Name: "VEVENT", Props: []propFilter{{Name: "SUMMARY", TextMatch: &textMatch{Text: "x"}}}}, false},
		{"property time-range start", compFilter{Name: "VEVENT", Props: []propFilter{{Name: "DTSTART", Start: at}}}, true},
		{"text on a property the instance writes", compFilter{Name: "VEVENT", Props: []propFilter{{Name: "RECURRENCE-ID", TextMatch: &textMatch{Text: "2026"}}}}, true},
		{"property time-range end", compFilter{Name: "VEVENT", Props: []propFilter{{Name: "DTSTART", End: at}}}, true},
		{"nested property time-range", compFilter{Name: "VEVENT", Comps: []compFilter{{Name: "VALARM", Props: []propFilter{{Name: "TRIGGER", Start: at}}}}}, true},
		{"nested component time-range", compFilter{Name: "VEVENT", Comps: []compFilter{{Name: "VALARM", Start: at}}}, true},
		{"nested component without one", compFilter{Name: "VEVENT", Comps: []compFilter{{Name: "VALARM", IsNotDefined: true}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readsInstanceTimes(&tt.filter); got != tt.want {
				t.Errorf("readsInstanceTimes = %v, want %v", got, tt.want)
			}
		})
	}
}

// A query reads an override as the instance expand writes, which names its
// original time with no RANGE.
func TestAQueryReadsARangedAnchorAsItsInstance(t *testing.T) {
	const ics = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Test//EN
BEGIN:VEVENT
UID:u1
DTSTAMP:20260101T000000Z
SUMMARY:Master
DTSTART:20260801T090000Z
DTEND:20260801T100000Z
RRULE:FREQ=DAILY;COUNT=10
END:VEVENT
BEGIN:VEVENT
UID:u1
DTSTAMP:20260101T000000Z
SUMMARY:Ranged
RECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z
DTSTART:20260803T110000Z
DTEND:20260803T120000Z
END:VEVENT
END:VCALENDAR`
	noRange := propFilter{Name: "RECURRENCE-ID", ParamFilter: []paramFilter{{Name: "RANGE", IsNotDefined: true}}}
	filter := &compFilter{Name: "VCALENDAR", Comps: []compFilter{{
		Name:  "VEVENT",
		Start: toDate(t, "20260803T110000Z"),
		End:   toDate(t, "20260803T120000Z"),
		Props: []propFilter{noRange},
	}}}
	co := &calendarObject{Path: "/u/c/e.ics", Data: parseCalendar(t, ics)}
	matched, err := matchObject(filter, co)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !matched {
		t.Errorf("matched = false, want the anchor read without its RANGE")
	}
}
