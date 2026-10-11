package caldav_test

import (
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

type crossLimit struct {
	window crossWindow
	status int
	// kept lists the SUMMARY of every component the report returns, in order.
	kept []string
}

// crossInstance names the properties one expanded instance must carry, each as
// "VALUE-TYPE:value"; an empty string requires the property to be absent.
type crossInstance map[string]string

// crossPaths states what each path that reads a recurrence answers for one
// stored object, so a change to how one path reads the object is held to the
// others.
type crossPaths struct {
	comp string
	ics  string

	put int

	queries []crossQuery

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
			if w.Code != http.StatusMultiStatus {
				t.Errorf("%v: status = %d, want %d\n%s", q.window, w.Code, http.StatusMultiStatus, w.Body.String())
				continue
			}
			if got := len(crossCalendarData(t, w.Body.Bytes())) == 1; got != q.matches {
				t.Errorf("%v: matched = %v, want %v", q.window, got, q.matches)
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
        <C:time-range ` + attrs + `/>
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
	prop := comp.Props.Get(name)
	if prop == nil {
		return ""
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
