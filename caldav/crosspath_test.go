package caldav_test

import (
	"cmp"
	"encoding/xml"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-ical"
	"github.com/mniehe/davkit/caldav"
)

const crossItem = "fixture.ics"

// crossWindow is a pair of RFC 4791 UTC times; an empty end leaves the window
// open where the element allows it.
type crossWindow struct{ start, end string }

type crossQuery struct {
	window  crossWindow
	matches bool
}

// crossTextQuery adds a SUMMARY text-match to the time-range, so the match
// is held to the properties of the instance the window finds.
type crossTextQuery struct {
	window  crossWindow
	summary string
	matches bool
}

type crossLimit struct {
	window crossWindow
	status int
	// kept lists the SUMMARY of every component the report returns, in order.
	kept []string
}

// crossInstance names the properties one expanded instance must carry, each as
// "VALUE-TYPE:value"; an empty string requires the property to be absent. A
// name of the form "PROP;PARAM" names that parameter's value instead.
type crossInstance map[string]string

// crossPaths states what each path that reads a recurrence answers for one
// stored object, so a change to how one path reads the object is held to the
// others.
type crossPaths struct {
	comp string
	ics  string

	put int

	queries     []crossQuery
	textQueries []crossTextQuery
	// queryStatus is what every query answers when it is not 207 Multi-Status.
	queryStatus int

	expand       crossWindow
	expandStatus int
	instances    []crossInstance

	freeBusy       crossWindow
	freeBusyStatus int
	busy           []string

	limits []crossLimit
}

var crossKinds = map[string]caldav.ItemKind{
	ical.CompEvent:   caldav.Event,
	ical.CompToDo:    caldav.Task,
	ical.CompJournal: caldav.Note,
}

func runCrossPaths(t *testing.T, c *crossPaths) {
	t.Helper()

	t.Run("PUT", func(t *testing.T) {
		h := handlerFor(t, newStore(t), caldav.Config{})
		if w := put(h, "/alice/work/"+crossItem, c.ics, nil); w.Code != c.put {
			t.Errorf("status = %d, want %d\n%s", w.Code, c.put, w.Body.String())
		}
	})

	store := newStore(t)
	seedKind(t, store, "alice", crossItem, c.ics, "fixture", crossKinds[c.comp])
	h := handlerFor(t, store, caldav.Config{})

	t.Run("calendar-query", func(t *testing.T) {
		for _, q := range c.queries {
			w := report(t, h, "/alice/work/", crossTimeRangeQuery(c.comp, q.window))
			want := cmp.Or(c.queryStatus, http.StatusMultiStatus)
			if w.Code != want {
				t.Errorf("%v: status = %d, want %d\n%s", q.window, w.Code, want, w.Body.String())
				continue
			}
			if w.Code != http.StatusMultiStatus {
				continue
			}
			if got := len(crossCalendarData(t, w.Body.Bytes())) == 1; got != q.matches {
				t.Errorf("%v: matched = %v, want %v", q.window, got, q.matches)
			}
		}
		for _, q := range c.textQueries {
			w := report(t, h, "/alice/work/", crossTextFilterQuery(c.comp, q.window, q.summary))
			if w.Code != http.StatusMultiStatus {
				t.Errorf("%v %q: status = %d, want %d\n%s", q.window, q.summary, w.Code, http.StatusMultiStatus, w.Body.String())
				continue
			}
			if got := len(crossCalendarData(t, w.Body.Bytes())) == 1; got != q.matches {
				t.Errorf("%v %q: matched = %v, want %v", q.window, q.summary, got, q.matches)
			}
		}
	})

	t.Run("expand", func(t *testing.T) {
		w := report(t, h, "/alice/work/", expandQuery(c.expand.start, c.expand.end))
		if w.Code != c.expandStatus {
			t.Fatalf("status = %d, want %d\n%s", w.Code, c.expandStatus, w.Body.String())
		}
		if w.Code != http.StatusMultiStatus {
			return
		}
		got := crossComponents(t, w.Body.Bytes(), c.comp)
		if len(got) != len(c.instances) {
			t.Fatalf("instances = %d, want %d", len(got), len(c.instances))
		}
		for i, want := range c.instances {
			for name, value := range want {
				if have := crossProp(got[i], name); have != value {
					t.Errorf("instance %d %s = %q, want %q", i, name, have, value)
				}
			}
		}
	})

	t.Run("free-busy", func(t *testing.T) {
		w := report(t, h, "/alice/work/", freeBusyQuery(c.freeBusy.start, c.freeBusy.end))
		if w.Code != c.freeBusyStatus {
			t.Fatalf("status = %d, want %d\n%s", w.Code, c.freeBusyStatus, w.Body.String())
		}
		if w.Code != http.StatusOK {
			return
		}
		if got := crossBusy(w.Body.String()); !slices.Equal(got, c.busy) {
			t.Errorf("busy = %q, want %q", got, c.busy)
		}
	})

	t.Run("limit-recurrence-set", func(t *testing.T) {
		for _, l := range c.limits {
			w := report(t, h, "/alice/work/", limitRecurrenceQuery(l.window.start, l.window.end))
			if w.Code != l.status {
				t.Errorf("%v: status = %d, want %d\n%s", l.window, w.Code, l.status, w.Body.String())
				continue
			}
			if w.Code != http.StatusMultiStatus {
				continue
			}
			comps := crossComponents(t, w.Body.Bytes(), c.comp)
			kept := make([]string, 0, len(comps))
			for _, comp := range comps {
				kept = append(kept, crossProp(comp, ical.PropSummary))
			}
			if !slices.Equal(kept, l.kept) {
				t.Errorf("%v: kept = %q, want %q", l.window, kept, l.kept)
			}
		}
	})
}

func crossTimeRangeQuery(comp string, window crossWindow) string {
	return crossFilterQuery(comp, window, "")
}

func crossTextFilterQuery(comp string, window crossWindow, summary string) string {
	return crossFilterQuery(comp, window, `
        <C:prop-filter name="SUMMARY"><C:text-match>`+summary+`</C:text-match></C:prop-filter>`)
}

func crossFilterQuery(comp string, window crossWindow, filters string) string {
	attrs := `start="` + window.start + `"`
	if window.end != "" {
		attrs += ` end="` + window.end + `"`
	}
	return `<?xml version="1.0"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><C:calendar-data/></D:prop>
  <C:filter>
    <C:comp-filter name="VCALENDAR">
      <C:comp-filter name="` + comp + `">
        <C:time-range ` + attrs + `/>` + filters + `
      </C:comp-filter>
    </C:comp-filter>
  </C:filter>
</C:calendar-query>`
}

// crossCalendarData returns the calendar-data of every response.
func crossCalendarData(t *testing.T, body []byte) []string {
	t.Helper()

	var ms struct {
		Responses []struct {
			Data string `xml:"propstat>prop>calendar-data"`
		} `xml:"response"`
	}
	if err := xml.Unmarshal(body, &ms); err != nil {
		t.Fatalf("decoding multistatus: %v\n%s", err, body)
	}
	data := make([]string, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		data = append(data, r.Data)
	}
	return data
}

func crossComponents(t *testing.T, body []byte, name string) []*ical.Component {
	t.Helper()

	data := crossCalendarData(t, body)
	if len(data) != 1 {
		t.Fatalf("responses = %d, want the fixture alone", len(data))
	}
	cal, err := ical.NewDecoder(strings.NewReader(data[0])).Decode()
	if err != nil {
		t.Fatalf("decoding calendar-data: %v\n%s", err, data[0])
	}
	var comps []*ical.Component
	for _, child := range cal.Children {
		if child.Name == name {
			comps = append(comps, child)
		}
	}
	return comps
}

func crossProp(comp *ical.Component, name string) string {
	name, param, isParam := strings.Cut(name, ";")
	prop := comp.Props.Get(name)
	if prop == nil {
		return ""
	}
	if isParam {
		return prop.Params.Get(param)
	}
	if name == ical.PropSummary {
		return prop.Value
	}
	return string(prop.ValueType()) + ":" + prop.Value
}

// crossBusy returns the value of every FREEBUSY property in order.
func crossBusy(body string) []string {
	var busy []string
	for _, line := range strings.Split(body, "\r\n") {
		if value, ok := strings.CutPrefix(line, "FREEBUSY:"); ok {
			busy = append(busy, value)
		}
	}
	return busy
}

// A child whose RECURRENCE-ID does not read cannot name an occurrence, so it
// takes none from its series: the master still matches where it occurs. New
// data of that shape is not valid calendar data (RFC 4791 §5.3.2.1).
func TestAnUnreadableInstanceIdentifierLeavesTheSeriesReadable(t *testing.T) {
	runCrossPaths(t, &crossPaths{
		comp: ical.CompEvent,
		ics: eventCalendar(
			"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:20260801T090000Z\r\nDTEND:20260801T100000Z\r\nRRULE:FREQ=DAILY;COUNT=5\r\n",
			"UID:fixture\r\nSUMMARY:Broken\r\nRECURRENCE-ID:not-a-date\r\nDTSTART:20260803T150000Z\r\nDTEND:20260803T160000Z\r\n",
		),
		put: http.StatusForbidden,
		queries: []crossQuery{
			{crossWindow{"20260803T090000Z", "20260803T100000Z"}, true},
			{crossWindow{"20260803T150000Z", "20260803T160000Z"}, true},
			{crossWindow{"20260810T000000Z", "20260811T000000Z"}, false},
		},
		// Expansion and free-busy refuse an override they cannot place, and
		// limit-recurrence-set fails once it needs the original time, as each
		// did before the series was matched.
		expand:         crossWindow{"20260801T000000Z", "20260901T000000Z"},
		expandStatus:   http.StatusForbidden,
		freeBusy:       crossWindow{"20260801T000000Z", "20260901T000000Z"},
		freeBusyStatus: http.StatusForbidden,
		limits: []crossLimit{
			{crossWindow{"20260803T150000Z", "20260803T160000Z"}, http.StatusMultiStatus, []string{"Master", "Broken"}},
			{crossWindow{"20260810T000000Z", "20260811T000000Z"}, http.StatusInternalServerError, nil},
		},
	})
}

// eventCalendar wraps VEVENT bodies, each given its DTSTAMP, in a VCALENDAR.
func eventCalendar(bodies ...string) string {
	return componentCalendar(ical.CompEvent, bodies...)
}

func componentCalendar(name string, bodies ...string) string {
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n"
	for _, body := range bodies {
		ics += "BEGIN:" + name + "\r\nDTSTAMP:20260801T000000Z\r\n" + body + "END:" + name + "\r\n"
	}
	return ics + "END:VCALENDAR\r\n"
}

// 00010101T000000Z is the instant Go's zero time names, so an occurrence there
// must not be taken for the end of a window that has none.
func TestAnOccurrenceAtTheZeroInstantDoesNotEndAnOpenWalk(t *testing.T) {
	runCrossPaths(t, &crossPaths{
		comp: ical.CompEvent,
		ics:  eventCalendar("UID:fixture\r\nSUMMARY:Ancient\r\nDTSTART:00010101T000000Z\r\nDTEND:00010101T010000Z\r\nRDATE:00020101T000000Z\r\n"),
		put:  http.StatusCreated,
		queries: []crossQuery{
			{crossWindow{"00011231T000000Z", ""}, true},
			{crossWindow{"00030101T000000Z", ""}, false},
			{crossWindow{"00011231T000000Z", "00020101T000000Z"}, false},
		},
		expand:       crossWindow{"00011231T000000Z", "00030101T000000Z"},
		expandStatus: http.StatusMultiStatus,
		instances: []crossInstance{{
			ical.PropDateTimeStart: "DATE-TIME:00020101T000000Z",
			ical.PropRecurrenceID:  "DATE-TIME:00020101T000000Z",
			ical.PropDateTimeEnd:   "DATE-TIME:00020101T010000Z",
		}},
		freeBusy:       crossWindow{"00011231T000000Z", "00030101T000000Z"},
		freeBusyStatus: http.StatusOK,
		busy:           []string{"00020101T000000Z/00020101T010000Z"},
		limits: []crossLimit{
			{crossWindow{"00011231T000000Z", "00030101T000000Z"}, http.StatusMultiStatus, []string{"Ancient"}},
		},
	})
}

// RFC 5545 §3.8.4.4 gives RECURRENCE-ID the value type of DTSTART, and
// §3.8.2.2 and §3.8.2.3 give DTEND and DUE the same, while §3.8.5.2 lets each
// RDATE be of any type. An expanded instance therefore takes the master's
// type: a DATE on a timed series is its midnight, and a time of day on an
// all-day series has no DATE to name it, so the expand report refuses the
// object. Matching, free-busy and limit-recurrence-set read the exact instant.
func TestMixedExtraDatesTakeTheMastersValueType(t *testing.T) {
	const (
		movedEvent = "UID:fixture\r\nSUMMARY:Moved\r\nRECURRENCE-ID:20260808T090000Z\r\nDTSTART;VALUE=DATE:20260901\r\n"
		wideFB     = "20260905T000000Z"
	)
	window := func(start, end string) crossWindow { return crossWindow{start, end} }
	fixtures := map[string]*crossPaths{
		"DATE-TIME event, DATE RDATE": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:20260801T090000Z\r\nDTEND:20260801T100000Z\r\nRDATE;VALUE=DATE:20260808,20260815\r\n",
				"UID:fixture\r\nSUMMARY:Moved\r\nRECURRENCE-ID:20260808T000000Z\r\nDTSTART:20260901T090000Z\r\nDTEND:20260901T100000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260815T003000Z", "20260815T010000Z"), true},
				{window("20260815T010000Z", "20260815T020000Z"), false},
				{window("20260808T000000Z", "20260808T010000Z"), false},
				{window("20260901T093000Z", "20260901T100000Z"), true},
			},
			expand:       window("20260815T000000Z", "20260816T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{{
				ical.PropDateTimeStart: "DATE-TIME:20260815T000000Z",
				ical.PropRecurrenceID:  "DATE-TIME:20260815T000000Z",
				ical.PropDateTimeEnd:   "DATE-TIME:20260815T010000Z",
			}},
			freeBusy:       window("20260805T000000Z", wideFB),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260815T000000Z/20260815T010000Z", "20260901T090000Z/20260901T100000Z"},
			limits: []crossLimit{
				{window("20260808T003000Z", "20260808T010000Z"), http.StatusMultiStatus, []string{"Master", "Moved"}},
				{window("20260808T010000Z", "20260808T020000Z"), http.StatusMultiStatus, []string{"Master"}},
			},
		},
		"DATE-TIME to-do, DATE RDATE": {
			comp: ical.CompToDo,
			ics: componentCalendar(ical.CompToDo,
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:20260801T090000Z\r\nDUE:20260801T170000Z\r\nRDATE;VALUE=DATE:20260808,20260815\r\n",
				"UID:fixture\r\nSUMMARY:Moved\r\nRECURRENCE-ID:20260808T000000Z\r\nDTSTART:20260901T090000Z\r\nDUE:20260901T170000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260815T060000Z", "20260815T070000Z"), true},
				{window("20260815T090000Z", "20260815T100000Z"), false},
				{window("20260808T060000Z", "20260808T070000Z"), false},
			},
			expand:       window("20260815T000000Z", "20260816T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{{
				ical.PropDateTimeStart: "DATE-TIME:20260815T000000Z",
				ical.PropRecurrenceID:  "DATE-TIME:20260815T000000Z",
				ical.PropDue:           "DATE-TIME:20260815T080000Z",
			}},
			// A to-do is no busy time (RFC 4791 §7.10).
			freeBusy:       window("20260805T000000Z", wideFB),
			freeBusyStatus: http.StatusOK,
			limits: []crossLimit{
				{window("20260808T060000Z", "20260808T070000Z"), http.StatusMultiStatus, []string{"Master", "Moved"}},
				{window("20260808T090000Z", "20260808T100000Z"), http.StatusMultiStatus, []string{"Master"}},
			},
		},
		"DATE event without an end, DATE-TIME RDATE": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART;VALUE=DATE:20260801\r\nRDATE:20260808T090000Z,20260815T090000Z\r\n",
				movedEvent,
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260816T080000Z", "20260816T083000Z"), true},
				{window("20260815T080000Z", "20260815T083000Z"), false},
				{window("20260816T090000Z", "20260816T100000Z"), false},
				{window("20260809T080000Z", "20260809T083000Z"), false},
			},
			expand:         window("20260801T000000Z", "20260901T000000Z"),
			expandStatus:   http.StatusForbidden,
			freeBusy:       window("20260805T000000Z", wideFB),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260815T090000Z/20260816T090000Z", "20260901T000000Z/20260902T000000Z"},
			limits: []crossLimit{
				{window("20260809T080000Z", "20260809T083000Z"), http.StatusMultiStatus, []string{"Master", "Moved"}},
				{window("20260808T080000Z", "20260808T083000Z"), http.StatusMultiStatus, []string{"Master"}},
			},
		},
		"DATE event lasting two days, DATE-TIME RDATE": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART;VALUE=DATE:20260801\r\nDTEND;VALUE=DATE:20260803\r\nRDATE:20260808T090000Z,20260815T090000Z\r\n",
				movedEvent,
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260817T080000Z", "20260817T083000Z"), true},
				{window("20260815T080000Z", "20260815T083000Z"), false},
				{window("20260817T090000Z", "20260817T100000Z"), false},
			},
			expand:         window("20260801T000000Z", "20260901T000000Z"),
			expandStatus:   http.StatusForbidden,
			freeBusy:       window("20260805T000000Z", wideFB),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260815T090000Z/20260817T090000Z", "20260901T000000Z/20260902T000000Z"},
			limits: []crossLimit{
				{window("20260810T080000Z", "20260810T083000Z"), http.StatusMultiStatus, []string{"Master", "Moved"}},
				{window("20260810T090000Z", "20260810T100000Z"), http.StatusMultiStatus, []string{"Master"}},
			},
		},
		"DATE event, PERIOD RDATE": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART;VALUE=DATE:20260801\r\nRDATE;VALUE=PERIOD:20260808T090000Z/PT3H,20260815T090000Z/PT2H\r\n",
				movedEvent,
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260815T103000Z", "20260815T104500Z"), true},
				{window("20260815T113000Z", "20260815T120000Z"), false},
				{window("20260808T100000Z", "20260808T103000Z"), false},
			},
			expand:         window("20260801T000000Z", "20260901T000000Z"),
			expandStatus:   http.StatusForbidden,
			freeBusy:       window("20260805T000000Z", wideFB),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260815T090000Z/20260815T110000Z", "20260901T000000Z/20260902T000000Z"},
			limits: []crossLimit{
				{window("20260808T113000Z", "20260808T114500Z"), http.StatusMultiStatus, []string{"Master", "Moved"}},
				{window("20260808T120000Z", "20260808T130000Z"), http.StatusMultiStatus, []string{"Master"}},
			},
		},
		"DATE event, DATE-TIME RDATE excluded": {
			comp: ical.CompEvent,
			ics:  eventCalendar("UID:fixture\r\nSUMMARY:Master\r\nDTSTART;VALUE=DATE:20260801\r\nRDATE:20260808T090000Z\r\nEXDATE:20260808T090000Z\r\nRDATE;VALUE=DATE:20260815\r\n"),
			put:  http.StatusCreated,
			queries: []crossQuery{
				{window("20260808T100000Z", "20260808T103000Z"), false},
				{window("20260815T100000Z", "20260815T103000Z"), true},
			},
			expand:       window("20260815T000000Z", "20260816T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{{
				ical.PropDateTimeStart: "DATE:20260815",
				ical.PropRecurrenceID:  "DATE:20260815",
				ical.PropDateTimeEnd:   "",
			}},
			freeBusy:       window("20260805T000000Z", wideFB),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260815T000000Z/20260816T000000Z"},
			limits: []crossLimit{
				{window("20260801T000000Z", "20260901T000000Z"), http.StatusMultiStatus, []string{"Master"}},
			},
		},
		"DATE journal, DATE-TIME RDATE": {
			comp: ical.CompJournal,
			ics: componentCalendar(ical.CompJournal,
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART;VALUE=DATE:20260801\r\nRDATE:20260808T090000Z,20260815T090000Z\r\n",
				movedEvent,
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260816T080000Z", "20260816T083000Z"), true},
				{window("20260815T080000Z", "20260815T083000Z"), false},
				{window("20260809T080000Z", "20260809T083000Z"), false},
			},
			expand:       window("20260801T000000Z", "20260901T000000Z"),
			expandStatus: http.StatusForbidden,
			// A journal entry is no busy time (RFC 4791 §7.10).
			freeBusy:       window("20260805T000000Z", wideFB),
			freeBusyStatus: http.StatusOK,
			limits: []crossLimit{
				{window("20260809T080000Z", "20260809T083000Z"), http.StatusMultiStatus, []string{"Master", "Moved"}},
				{window("20260808T080000Z", "20260808T083000Z"), http.StatusMultiStatus, []string{"Master"}},
			},
		},
		"DATE journal, DATE override": {
			comp: ical.CompJournal,
			ics: componentCalendar(ical.CompJournal,
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART;VALUE=DATE:20260801\r\nRRULE:FREQ=WEEKLY;COUNT=3\r\n",
				"UID:fixture\r\nSUMMARY:Moved\r\nRECURRENCE-ID;VALUE=DATE:20260808\r\nDTSTART;VALUE=DATE:20260901\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260808T120000Z", "20260808T130000Z"), false},
				{window("20260815T120000Z", "20260815T130000Z"), true},
				{window("20260901T120000Z", "20260901T130000Z"), true},
			},
			expand:       window("20260815T000000Z", "20260816T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{{
				ical.PropDateTimeStart: "DATE:20260815",
				ical.PropRecurrenceID:  "DATE:20260815",
			}},
			freeBusy:       window("20260805T000000Z", wideFB),
			freeBusyStatus: http.StatusOK,
			limits: []crossLimit{
				{window("20260808T120000Z", "20260808T130000Z"), http.StatusMultiStatus, []string{"Master", "Moved"}},
				{window("20260809T000000Z", "20260809T010000Z"), http.StatusMultiStatus, []string{"Master"}},
			},
		},
	}
	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			runCrossPaths(t, fixture)
		})
	}
}

// backwardMonth moves the daily series from 10 August on back 31 days.
const backwardMonth = "UID:fixture\r\nSUMMARY:Moved\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260810T090000Z\r\nDTSTART:20260710T090000Z\r\nDTEND:20260710T100000Z\r\n"

// RFC 4791 §9.6.6 keeps the overrides that affect the window so that a client
// can expand the limited object itself; a range moving an occurrence into the
// window from after it is one of those.
func TestALimitedObjectStillYieldsWhatABackwardRangeMovesIn(t *testing.T) {
	ics := eventCalendar(
		"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:20260801T090000Z\r\nDTEND:20260801T100000Z\r\nRRULE:FREQ=DAILY;COUNT=60\r\nEXDATE:20260805T090000Z\r\n",
		backwardMonth,
	)
	store := newStore(t)
	seedKind(t, store, "alice", crossItem, ics, "fixture", caldav.Event)
	h := handlerFor(t, store, caldav.Config{})
	w := report(t, h, "/alice/work/", limitRecurrenceQuery("20260805T000000Z", "20260806T000000Z"))
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("limit status = %d\n%s", w.Code, w.Body.String())
	}
	limited := crossCalendarData(t, w.Body.Bytes())
	if len(limited) != 1 {
		t.Fatalf("responses = %d, want 1", len(limited))
	}

	again := newStore(t)
	seedKind(t, again, "alice", crossItem, limited[0], "fixture", caldav.Event)
	w = report(t, handlerFor(t, again, caldav.Config{}), "/alice/work/", expandQuery("20260805T000000Z", "20260806T000000Z"))
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("expand status = %d\n%s", w.Code, w.Body.String())
	}
	got := crossComponents(t, w.Body.Bytes(), ical.CompEvent)
	if len(got) != 1 {
		t.Fatalf("instances of the limited object = %d, want 1", len(got))
	}
	if start, original := crossProp(got[0], ical.PropDateTimeStart), crossProp(got[0], ical.PropRecurrenceID); start != "DATE-TIME:20260805T090000Z" || original != "DATE-TIME:20260905T090000Z" {
		t.Errorf("instance = %s from %s, want DATE-TIME:20260805T090000Z from DATE-TIME:20260905T090000Z", start, original)
	}
}

// RFC 5545 §3.8.4.4: an override whose RECURRENCE-ID carries
// RANGE=THISANDFUTURE redefines every later instance too, each moved by the
// same time difference and given the override's length and properties. Every
// path that reads the series reads the instances it describes, and an expanded
// instance names its original start with no RANGE (RFC 4791 §9.6.5).
func TestRangedOverridesGovernTheOccurrencesAfterThem(t *testing.T) {
	const (
		rid       = ical.PropRecurrenceID
		ridRange  = ical.PropRecurrenceID + ";" + ical.ParamRange
		ridReview = ical.PropRecurrenceID + ";X-REVIEW"
		dtstart   = ical.PropDateTimeStart
		dtend     = ical.PropDateTimeEnd
		summary   = ical.PropSummary
		daily     = "UID:fixture\r\nSUMMARY:Master\r\nDTSTART:20260801T090000Z\r\nDTEND:20260801T100000Z\r\nRRULE:FREQ=DAILY;COUNT=10\r\n"
		laterAt   = "UID:fixture\r\nSUMMARY:Ranged\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z\r\nDTSTART:20260803T110000Z\r\nDTEND:20260803T120000Z\r\n"
		excluded  = "UID:fixture\r\nSUMMARY:Master\r\nDTSTART:20260801T090000Z\r\nDTEND:20260801T100000Z\r\nRRULE:FREQ=DAILY;COUNT=10\r\nEXDATE:20260805T090000Z\r\n"
	)
	window := func(start, end string) crossWindow { return crossWindow{start, end} }
	shifted := func(start, end, original, name string) crossInstance {
		return crossInstance{
			dtstart:                  "DATE-TIME:" + start,
			dtend:                    "DATE-TIME:" + end,
			rid:                      "DATE-TIME:" + original,
			ridRange:                 "",
			summary:                  name,
			ical.PropRecurrenceRule:  "",
			ical.PropRecurrenceDates: "",
			ical.PropExceptionDates:  "",
		}
	}
	fixtures := map[string]*crossPaths{
		"later shift with its own length and summary": {
			comp: ical.CompEvent,
			ics: eventCalendar(daily,
				"UID:fixture\r\nSUMMARY:Ranged\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z\r\nDTSTART:20260803T110000Z\r\nDTEND:20260803T130000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260805T110000Z", "20260805T120000Z"), true},
				{window("20260805T090000Z", "20260805T100000Z"), false},
				{window("20260802T090000Z", "20260802T100000Z"), true},
				{window("20260811T110000Z", "20260811T120000Z"), false},
			},
			textQueries: []crossTextQuery{
				{window("20260805T120000Z", "20260805T130000Z"), "Ranged", true},
				{window("20260805T120000Z", "20260805T130000Z"), "Master", false},
				{window("20260802T090000Z", "20260802T100000Z"), "Master", true},
			},
			expand:       window("20260803T000000Z", "20260806T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{
				shifted("20260803T110000Z", "20260803T130000Z", "20260803T090000Z", "Ranged"),
				shifted("20260804T110000Z", "20260804T130000Z", "20260804T090000Z", "Ranged"),
				shifted("20260805T110000Z", "20260805T130000Z", "20260805T090000Z", "Ranged"),
			},
			freeBusy:       window("20260803T000000Z", "20260806T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260803T110000Z/20260803T130000Z", "20260804T110000Z/20260804T130000Z", "20260805T110000Z/20260805T130000Z"},
			limits: []crossLimit{
				{window("20260805T110000Z", "20260805T120000Z"), http.StatusMultiStatus, []string{"Master", "Ranged"}},
				{window("20260802T090000Z", "20260802T100000Z"), http.StatusMultiStatus, []string{"Master"}},
			},
		},
		"earlier shift moves an occurrence after the window into it": {
			comp: ical.CompEvent,
			ics: eventCalendar(daily,
				"UID:fixture\r\nSUMMARY:Earlier\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z\r\nDTSTART:20260803T030000Z\r\nDTEND:20260803T040000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260805T030000Z", "20260805T040000Z"), true},
				{window("20260805T090000Z", "20260805T100000Z"), false},
			},
			expand:       window("20260805T030000Z", "20260805T040000Z"),
			expandStatus: http.StatusMultiStatus,
			instances:    []crossInstance{shifted("20260805T030000Z", "20260805T040000Z", "20260805T090000Z", "Earlier")},
			freeBusy:     window("20260805T000000Z", "20260806T000000Z"),
			// The window ends before the next original start is shifted in.
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260805T030000Z/20260805T040000Z"},
			limits: []crossLimit{
				{window("20260805T030000Z", "20260805T040000Z"), http.StatusMultiStatus, []string{"Master", "Earlier"}},
			},
		},
		"an exact override interrupts one range, a later range takes over": {
			comp: ical.CompEvent,
			ics: eventCalendar(daily,
				"UID:fixture\r\nSUMMARY:A\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z\r\nDTSTART:20260803T100000Z\r\nDTEND:20260803T110000Z\r\n",
				"UID:fixture\r\nSUMMARY:X\r\nRECURRENCE-ID:20260805T090000Z\r\nDTSTART:20260805T150000Z\r\nDTEND:20260805T160000Z\r\n",
				"UID:fixture\r\nSUMMARY:B\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260807T090000Z\r\nDTSTART:20260807T070000Z\r\nDTEND:20260807T080000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260806T100000Z", "20260806T110000Z"), true},
				{window("20260806T090000Z", "20260806T100000Z"), false},
				{window("20260805T100000Z", "20260805T110000Z"), false},
				{window("20260805T150000Z", "20260805T160000Z"), true},
				{window("20260808T070000Z", "20260808T080000Z"), true},
				{window("20260808T100000Z", "20260808T110000Z"), false},
			},
			expand:       window("20260802T000000Z", "20260809T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{
				shifted("20260802T090000Z", "20260802T100000Z", "20260802T090000Z", "Master"),
				shifted("20260803T100000Z", "20260803T110000Z", "20260803T090000Z", "A"),
				shifted("20260804T100000Z", "20260804T110000Z", "20260804T090000Z", "A"),
				shifted("20260805T150000Z", "20260805T160000Z", "20260805T090000Z", "X"),
				shifted("20260806T100000Z", "20260806T110000Z", "20260806T090000Z", "A"),
				shifted("20260807T070000Z", "20260807T080000Z", "20260807T090000Z", "B"),
				shifted("20260808T070000Z", "20260808T080000Z", "20260808T090000Z", "B"),
			},
			freeBusy:       window("20260802T000000Z", "20260809T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy: []string{
				"20260802T090000Z/20260802T100000Z", "20260803T100000Z/20260803T110000Z",
				"20260804T100000Z/20260804T110000Z", "20260805T150000Z/20260805T160000Z",
				"20260806T100000Z/20260806T110000Z", "20260807T070000Z/20260807T080000Z",
				"20260808T070000Z/20260808T080000Z",
			},
		},
		"EXDATE removes an occurrence the range would have moved": {
			comp: ical.CompEvent,
			ics:  eventCalendar(excluded, laterAt),
			put:  http.StatusCreated,
			queries: []crossQuery{
				{window("20260805T090000Z", "20260805T100000Z"), false},
				{window("20260805T110000Z", "20260805T120000Z"), false},
				{window("20260806T110000Z", "20260806T120000Z"), true},
			},
			expand:         window("20260805T000000Z", "20260807T000000Z"),
			expandStatus:   http.StatusMultiStatus,
			instances:      []crossInstance{shifted("20260806T110000Z", "20260806T120000Z", "20260806T090000Z", "Ranged")},
			freeBusy:       window("20260805T000000Z", "20260807T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260806T110000Z/20260806T120000Z"},
		},
		"an exact override at an EXDATE inside a range survives": {
			comp: ical.CompEvent,
			ics: eventCalendar(excluded, laterAt,
				"UID:fixture\r\nSUMMARY:Kept\r\nRECURRENCE-ID:20260805T090000Z\r\nDTSTART:20260805T200000Z\r\nDTEND:20260805T210000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260805T200000Z", "20260805T210000Z"), true},
				{window("20260805T110000Z", "20260805T120000Z"), false},
			},
			expand:       window("20260805T000000Z", "20260807T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{
				shifted("20260806T110000Z", "20260806T120000Z", "20260806T090000Z", "Ranged"),
				shifted("20260805T200000Z", "20260805T210000Z", "20260805T090000Z", "Kept"),
			},
			freeBusy:       window("20260805T000000Z", "20260807T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260805T200000Z/20260805T210000Z", "20260806T110000Z/20260806T120000Z"},
		},
		"to-do takes the range's start and due": {
			comp: ical.CompToDo,
			ics: componentCalendar(ical.CompToDo,
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:20260801T090000Z\r\nDUE:20260801T100000Z\r\nRRULE:FREQ=DAILY;COUNT=10\r\n",
				"UID:fixture\r\nSUMMARY:Ranged\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z\r\nDTSTART:20260803T120000Z\r\nDUE:20260803T160000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260805T140000Z", "20260805T150000Z"), true},
				{window("20260805T090000Z", "20260805T100000Z"), false},
			},
			textQueries: []crossTextQuery{
				{window("20260805T140000Z", "20260805T150000Z"), "Ranged", true},
			},
			expand:       window("20260805T000000Z", "20260806T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{{
				dtstart:                 "DATE-TIME:20260805T120000Z",
				ical.PropDue:            "DATE-TIME:20260805T160000Z",
				rid:                     "DATE-TIME:20260805T090000Z",
				ridRange:                "",
				summary:                 "Ranged",
				ical.PropRecurrenceRule: "",
			}},
			// A to-do is no busy time (RFC 4791 §7.10).
			freeBusy:       window("20260805T000000Z", "20260806T000000Z"),
			freeBusyStatus: http.StatusOK,
		},
		// Across the end of daylight time the difference is 25 hours, so the
		// next instance lands an hour later on the wall clock than a move by
		// one day would put it.
		"the shift is an absolute difference, not a wall-clock one": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART;TZID=America/Los_Angeles:20261024T090000\r\nDTEND;TZID=America/Los_Angeles:20261024T100000\r\nRRULE:FREQ=WEEKLY;COUNT=4\r\n",
				"UID:fixture\r\nSUMMARY:Sunday\r\nRECURRENCE-ID;TZID=America/Los_Angeles;RANGE=THISANDFUTURE:20261031T090000\r\nDTSTART;TZID=America/Los_Angeles:20261101T090000\r\nDTEND;TZID=America/Los_Angeles:20261101T100000\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20261108T180000Z", "20261108T190000Z"), true},
				{window("20261108T170000Z", "20261108T173000Z"), false},
			},
			expand:         window("20261107T000000Z", "20261110T000000Z"),
			expandStatus:   http.StatusMultiStatus,
			instances:      []crossInstance{shifted("20261108T180000Z", "20261108T190000Z", "20261107T170000Z", "Sunday")},
			freeBusy:       window("20261107T000000Z", "20261110T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20261108T180000Z/20261108T190000Z"},
		},
		"all-day range writes no RANGE on the instances it yields": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART;VALUE=DATE:20260801\r\nRRULE:FREQ=DAILY;COUNT=10\r\n",
				"UID:fixture\r\nSUMMARY:Ranged\r\nRECURRENCE-ID;VALUE=DATE;RANGE=THISANDFUTURE:20260803\r\nDTSTART;VALUE=DATE:20260804\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260803T120000Z", "20260803T130000Z"), false},
				{window("20260804T120000Z", "20260804T130000Z"), true},
			},
			expand:       window("20260803T000000Z", "20260806T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{
				{dtstart: "DATE:20260804", rid: "DATE:20260803", ridRange: "", summary: "Ranged", ical.PropRecurrenceRule: ""},
				{dtstart: "DATE:20260805", rid: "DATE:20260804", ridRange: "", summary: "Ranged", ical.PropRecurrenceRule: ""},
			},
			freeBusy:       window("20260803T000000Z", "20260806T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260804T000000Z/20260806T000000Z"},
		},
		"an all-day range on the last occurrence keeps no RANGE": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART;VALUE=DATE:20260801\r\nRRULE:FREQ=DAILY;COUNT=3\r\n",
				"UID:fixture\r\nSUMMARY:Ranged\r\nRECURRENCE-ID;VALUE=DATE;RANGE=THISANDFUTURE:20260803\r\nDTSTART;VALUE=DATE:20260804\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260804T120000Z", "20260804T130000Z"), true},
			},
			expand:       window("20260804T000000Z", "20260805T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{
				{dtstart: "DATE:20260804", rid: "DATE:20260803", ridRange: "", summary: "Ranged"},
			},
			freeBusy:       window("20260804T000000Z", "20260805T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260804T000000Z/20260805T000000Z"},
		},
		// Compatibility extension, not an RFC requirement: davkit keeps a
		// detached instance the rule never generates, and its range still
		// governs the later occurrences.
		"compatibility: a range anchored where the rule never occurs": {
			comp: ical.CompEvent,
			ics: eventCalendar(daily,
				"UID:fixture\r\nSUMMARY:Detached\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260803T120000Z\r\nDTSTART:20260803T140000Z\r\nDTEND:20260803T150000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260804T110000Z", "20260804T120000Z"), true},
				{window("20260804T090000Z", "20260804T100000Z"), false},
				{window("20260803T090000Z", "20260803T100000Z"), true},
				{window("20260803T140000Z", "20260803T150000Z"), true},
			},
			expand:       window("20260803T000000Z", "20260805T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{
				shifted("20260803T090000Z", "20260803T100000Z", "20260803T090000Z", "Master"),
				shifted("20260804T110000Z", "20260804T120000Z", "20260804T090000Z", "Detached"),
				shifted("20260803T140000Z", "20260803T150000Z", "20260803T120000Z", "Detached"),
			},
			freeBusy:       window("20260803T000000Z", "20260805T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260803T090000Z/20260803T100000Z", "20260803T140000Z/20260803T150000Z", "20260804T110000Z/20260804T120000Z"},
		},
		// Compatibility extension: an all-day range on a timed series yields
		// all-day instances, each still named by its timed original.
		"compatibility: an all-day range on a timed series": {
			comp: ical.CompEvent,
			ics: eventCalendar(daily,
				"UID:fixture\r\nSUMMARY:AllDay\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z\r\nDTSTART;VALUE=DATE:20260803\r\nDTEND;VALUE=DATE:20260804\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260805T150000Z", "20260805T160000Z"), true},
				{window("20260802T150000Z", "20260802T160000Z"), false},
			},
			expand:       window("20260805T000000Z", "20260806T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{{
				dtstart:  "DATE:20260805",
				dtend:    "DATE:20260806",
				rid:      "DATE-TIME:20260805T090000Z",
				ridRange: "",
				summary:  "AllDay",
			}},
			freeBusy:       window("20260805T000000Z", "20260806T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260805T000000Z/20260806T000000Z"},
		},
		"a range with no start is refused, not ignored": {
			comp: ical.CompEvent,
			ics: eventCalendar(daily,
				"UID:fixture\r\nSUMMARY:Broken\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z\r\nDTEND:20260803T130000Z\r\n",
			),
			put: http.StatusForbidden,
			queries: []crossQuery{
				{window("20260802T090000Z", "20260802T100000Z"), false},
				{window("20260805T090000Z", "20260805T100000Z"), false},
			},
			queryStatus:    http.StatusInternalServerError,
			expand:         window("20260801T000000Z", "20260901T000000Z"),
			expandStatus:   http.StatusForbidden,
			freeBusy:       window("20260801T000000Z", "20260901T000000Z"),
			freeBusyStatus: http.StatusForbidden,
		},
		"a range whose end does not read is refused, not ignored": {
			comp: ical.CompEvent,
			ics: eventCalendar(daily,
				"UID:fixture\r\nSUMMARY:Broken\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260803T090000Z\r\nDTSTART:20260803T110000Z\r\nDURATION:soon\r\n",
			),
			put: http.StatusForbidden,
			queries: []crossQuery{
				{window("20260805T090000Z", "20260805T100000Z"), false},
			},
			queryStatus:    http.StatusInternalServerError,
			expand:         window("20260801T000000Z", "20260901T000000Z"),
			expandStatus:   http.StatusForbidden,
			freeBusy:       window("20260801T000000Z", "20260901T000000Z"),
			freeBusyStatus: http.StatusForbidden,
		},
		// The original that lands in the window starts a month after it.
		"a large earlier shift is found by looking ahead": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:20260801T090000Z\r\nDTEND:20260801T100000Z\r\nRRULE:FREQ=DAILY;COUNT=100\r\n",
				"UID:fixture\r\nSUMMARY:Back\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260810T090000Z\r\nDTSTART:20260711T030000Z\r\nDTEND:20260711T040000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260901T030000Z", "20260901T040000Z"), true},
				{window("20260901T090000Z", "20260901T100000Z"), false},
			},
			expand:         window("20260901T030000Z", "20260901T040000Z"),
			expandStatus:   http.StatusMultiStatus,
			instances:      []crossInstance{shifted("20260901T030000Z", "20260901T040000Z", "20261001T090000Z", "Back")},
			freeBusy:       window("20260901T000000Z", "20260902T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260901T030000Z/20260901T040000Z"},
		},
		// The backward range is superseded by a later one long before the
		// window, so it moves nothing the window could see and the walk need not
		// look as far as it would carry an occurrence.
		"a superseded backward range does not widen the walk": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:20260801T090000Z\r\nDTEND:20260801T090100Z\r\nRRULE:FREQ=MINUTELY\r\n",
				"UID:fixture\r\nSUMMARY:Back\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260802T090000Z\r\nDTSTART:20260501T090000Z\r\nDTEND:20260501T090100Z\r\n",
				"UID:fixture\r\nSUMMARY:Again\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260802T100000Z\r\nDTSTART:20260802T100000Z\r\nDTEND:20260802T100100Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260804T090000Z", "20260804T090200Z"), true},
			},
			expand:       window("20260804T090000Z", "20260804T090200Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{
				shifted("20260804T090000Z", "20260804T090100Z", "20260804T090000Z", "Again"),
				shifted("20260804T090100Z", "20260804T090200Z", "20260804T090100Z", "Again"),
			},
			freeBusy:       window("20260804T090000Z", "20260804T090200Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260804T090000Z/20260804T090200Z"},
		},
		// Two ranges share one anchor; the later governs, so the earlier's year
		// backward moves nothing and must not widen the walk.
		"a range superseded at its own anchor does not widen the walk": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:20260801T000000Z\r\nRRULE:FREQ=MINUTELY\r\n",
				"UID:fixture\r\nSUMMARY:Year\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20261201T000000Z\r\nDTSTART:20251201T000000Z\r\n",
				"UID:fixture\r\nSUMMARY:Same\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20261201T000000Z\r\nDTSTART:20261201T000000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260802T000030Z", "20260802T000031Z"), false},
			},
			expand:         window("20260802T000030Z", "20260802T000031Z"),
			expandStatus:   http.StatusMultiStatus,
			freeBusy:       window("20260802T000030Z", "20260802T000031Z"),
			freeBusyStatus: http.StatusOK,
		},
		// The second original would move into year 10000, which no
		// iCalendar time can name, so it yields no instance.
		"an occurrence moved past the last writable year yields nothing": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:99991230T000000Z\r\nDTEND:99991230T010000Z\r\nRRULE:FREQ=DAILY;COUNT=2\r\n",
				"UID:fixture\r\nSUMMARY:Last\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:99991230T000000Z\r\nDTSTART:99991231T000000Z\r\nDTEND:99991231T010000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("99991231T000000Z", "99991231T120000Z"), true},
			},
			expand:         window("99991230T000000Z", "99991231T235959Z"),
			expandStatus:   http.StatusMultiStatus,
			instances:      []crossInstance{shifted("99991231T000000Z", "99991231T010000Z", "99991230T000000Z", "Last")},
			freeBusy:       window("99991230T000000Z", "99991231T235959Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"99991231T000000Z/99991231T010000Z"},
		},
		// The range moves the 5 September original back a month, into a window
		// its RECURRENCE-ID lies after, so limit-recurrence-set keeps it.
		"limit-recurrence-set keeps a backward range for the window it moves into": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:20260801T090000Z\r\nDTEND:20260801T100000Z\r\nRRULE:FREQ=DAILY;COUNT=60\r\nEXDATE:20260805T090000Z\r\n",
				backwardMonth,
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260805T090000Z", "20260805T100000Z"), true},
			},
			expand:         window("20260805T000000Z", "20260806T000000Z"),
			expandStatus:   http.StatusMultiStatus,
			instances:      []crossInstance{shifted("20260805T090000Z", "20260805T100000Z", "20260905T090000Z", "Moved")},
			freeBusy:       window("20260805T000000Z", "20260806T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260805T090000Z/20260805T100000Z"},
			limits: []crossLimit{
				{window("20260805T000000Z", "20260806T000000Z"), http.StatusMultiStatus, []string{"Master", "Moved"}},
			},
		},
		"limit-recurrence-set drops a forward range anchored after the window": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:20260801T090000Z\r\nDTEND:20260801T100000Z\r\nRRULE:FREQ=DAILY;COUNT=60\r\n",
				"UID:fixture\r\nSUMMARY:Later\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260810T090000Z\r\nDTSTART:20260810T110000Z\r\nDTEND:20260810T120000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260805T090000Z", "20260805T100000Z"), true},
			},
			expand:         window("20260805T000000Z", "20260806T000000Z"),
			expandStatus:   http.StatusMultiStatus,
			instances:      []crossInstance{shifted("20260805T090000Z", "20260805T100000Z", "20260805T090000Z", "Master")},
			freeBusy:       window("20260805T000000Z", "20260806T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260805T090000Z/20260805T100000Z"},
			limits: []crossLimit{
				{window("20260805T000000Z", "20260806T000000Z"), http.StatusMultiStatus, []string{"Master"}},
			},
		},
		// A move of eight thousand years exceeds what time.Duration holds.
		"a range moving its series millennia ahead": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:10010101T090000Z\r\nDTEND:10010101T100000Z\r\nRRULE:FREQ=YEARLY;COUNT=3\r\n",
				"UID:fixture\r\nSUMMARY:Far\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:10010101T090000Z\r\nDTSTART:90000101T090000Z\r\nDTEND:90000101T100000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("90010101T090000Z", "90010101T100000Z"), true},
				{window("10020101T090000Z", "10020101T100000Z"), false},
			},
			expand:         window("90010101T000000Z", "90010102T000000Z"),
			expandStatus:   http.StatusMultiStatus,
			instances:      []crossInstance{shifted("90010101T090000Z", "90010101T100000Z", "10020101T090000Z", "Far")},
			freeBusy:       window("90010101T000000Z", "90010102T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"90010101T090000Z/90010101T100000Z"},
		},
		// The occurrence lies over three centuries after the anchor, further
		// than time.Duration reaches. It is an RDATE because rrule-go stops a
		// rule after about 292 years.
		"a small range applied centuries after its anchor": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:17000801T090000Z\r\nDTEND:17000801T100000Z\r\nRDATE:20260801T090000Z\r\n",
				"UID:fixture\r\nSUMMARY:Hour\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:17000801T090000Z\r\nDTSTART:17000801T100000Z\r\nDTEND:17000801T110000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260801T103000Z", "20260801T104500Z"), true},
				{window("20260801T090000Z", "20260801T100000Z"), false},
			},
			expand:         window("20260801T000000Z", "20260802T000000Z"),
			expandStatus:   http.StatusMultiStatus,
			instances:      []crossInstance{shifted("20260801T100000Z", "20260801T110000Z", "20260801T090000Z", "Hour")},
			freeBusy:       window("20260801T000000Z", "20260802T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260801T100000Z/20260801T110000Z"},
		},
		"a small to-do range applied centuries after its anchor": {
			comp: ical.CompToDo,
			ics: componentCalendar(ical.CompToDo,
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:17000801T090000Z\r\nDUE:17000801T100000Z\r\nRDATE:20260801T090000Z\r\n",
				"UID:fixture\r\nSUMMARY:Hours\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:17000801T090000Z\r\nDTSTART:17000801T100000Z\r\nDUE:17000801T120000Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260801T113000Z", "20260801T114500Z"), true},
				{window("20260801T123000Z", "20260801T124500Z"), false},
			},
			expand:       window("20260801T000000Z", "20260802T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{{
				dtstart:      "DATE-TIME:20260801T100000Z",
				ical.PropDue: "DATE-TIME:20260801T120000Z",
				rid:          "DATE-TIME:20260801T090000Z",
			}},
			freeBusy:       window("20260801T000000Z", "20260802T000000Z"),
			freeBusyStatus: http.StatusOK,
		},
		// Parameters other than those the writer decides travel with the
		// RECURRENCE-ID, as they did when an override was copied whole.
		"an override's own RECURRENCE-ID parameters are kept": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART;VALUE=DATE:20260801\r\nRRULE:FREQ=DAILY;COUNT=10\r\n",
				"UID:fixture\r\nSUMMARY:Exact\r\nRECURRENCE-ID;VALUE=DATE;X-REVIEW=kept:20260803\r\nDTSTART;VALUE=DATE:20260803\r\n",
				"UID:fixture\r\nSUMMARY:Ranged\r\nRECURRENCE-ID;VALUE=DATE;RANGE=THISANDFUTURE;X-REVIEW=ranged:20260805\r\nDTSTART;VALUE=DATE:20260806\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260806T120000Z", "20260806T130000Z"), true},
			},
			expand:       window("20260803T000000Z", "20260807T000000Z"),
			expandStatus: http.StatusMultiStatus,
			instances: []crossInstance{
				{rid: "DATE:20260803", ridReview: "kept", summary: "Exact"},
				{rid: "DATE:20260804", ridReview: "", summary: "Master"},
				{rid: "DATE:20260805", ridReview: "ranged", ridRange: "", summary: "Ranged"},
			},
			freeBusy:       window("20260803T000000Z", "20260807T000000Z"),
			freeBusyStatus: http.StatusOK,
			busy:           []string{"20260803T000000Z/20260805T000000Z", "20260806T000000Z/20260807T000000Z"},
		},
		// Looking that far ahead exceeds the iteration cap, which is reported
		// as such rather than answered from the part that was walked.
		"a look-ahead past the iteration cap overflows": {
			comp: ical.CompEvent,
			ics: eventCalendar(
				"UID:fixture\r\nSUMMARY:Master\r\nDTSTART:20260801T090000Z\r\nDTEND:20260801T090100Z\r\nRRULE:FREQ=MINUTELY\r\n",
				"UID:fixture\r\nSUMMARY:Back\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20260802T090000Z\r\nDTSTART:20260501T090000Z\r\nDTEND:20260501T090100Z\r\n",
			),
			put: http.StatusCreated,
			queries: []crossQuery{
				{window("20260803T090000Z", "20260803T100000Z"), false},
			},
			queryStatus:    http.StatusInsufficientStorage,
			expand:         window("20260803T090000Z", "20260803T100000Z"),
			expandStatus:   http.StatusForbidden,
			freeBusy:       window("20260803T090000Z", "20260803T100000Z"),
			freeBusyStatus: http.StatusForbidden,
		},
	}
	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			runCrossPaths(t, fixture)
		})
	}
}
