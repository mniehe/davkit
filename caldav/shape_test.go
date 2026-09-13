package caldav_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mniehe/davkit/caldav"
	"github.com/mniehe/davkit/caldavmem"
)

// Far past any limit a real calendar needs, so these hold whatever the exact
// bounds are.
const (
	hostileDepth      = 64
	hostileComponents = 5000
	hostileParamBytes = 64 << 10
)

// eventWith is a storable event carrying lines inside its VEVENT.
func eventWith(lines ...string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:shape\r\n" +
		"DTSTAMP:20260801T000000Z\r\nDTSTART:20260902T100000Z\r\n" +
		strings.Join(lines, "\r\n") + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

// nestedWith opens levels of X- components, each BEGIN spelled by begin.
func nestedWith(levels int, begin string) string {
	lines := make([]string, 0, 2*levels)
	for i := range levels {
		lines = append(lines, fmt.Sprintf("%sX-LEVEL-%d", begin, i))
	}
	for i := levels - 1; i >= 0; i-- {
		lines = append(lines, fmt.Sprintf("END:X-LEVEL-%d", i))
	}
	return eventWith(lines...)
}

func alarms(count int) string {
	lines := make([]string, 0, 4*count)
	for range count {
		lines = append(lines, "BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT15M", "END:VALARM")
	}
	return eventWith(lines...)
}

// go-ical recurses once per nested component and builds each parameter value
// a byte at a time, so the shape has to be refused before it decodes. Each
// BEGIN spelling here is one go-ical reads as BEGIN.
func TestPutRefusesAHostileShapeBeforeParsing(t *testing.T) {
	h := handlerFor(t, newStore(t), caldav.Config{})
	longValue := strings.Repeat("a", hostileParamBytes)

	tests := map[string]string{
		"deep nesting":                         nestedWith(hostileDepth, "BEGIN:"),
		"deep nesting, folded BEGIN":           nestedWith(hostileDepth, "BEG\r\n IN:"),
		"deep nesting, BEGIN with a parameter": nestedWith(hostileDepth, "BEGIN;X-P=1:"),
		"deep nesting, lower case":             nestedWith(hostileDepth, "begin:"),
		"deep nesting, dotless i":              nestedWith(hostileDepth, "BEGıN:"),
		"too many components":                  alarms(hostileComponents),
		"a long parameter value":               eventWith("X-A;X-P=" + longValue + ":v"),
		"a long quoted parameter value":        eventWith(`X-A;X-P="` + longValue + `":v`),
		"bytes that are not UTF-8":             eventWith("SUMMARY:Caf\xe9"),
		// Both of these panic go-ical's parameter decoder.
		"a parameter ending its line":   eventWith("X-A;X-P=v"),
		"text after a quoted parameter": eventWith(`X-A;X-P="v"x:y`),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			w := put(h, "/alice/work/shape.ics", body, nil)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
			}
			if !strings.Contains(w.Body.String(), "valid-calendar-data") {
				t.Errorf("body = %q, want the CALDAV:valid-calendar-data precondition", w.Body.String())
			}
		})
	}
}

func TestPutAcceptsTheShapesRealCalendarsHave(t *testing.T) {
	h := handlerFor(t, newStore(t), caldav.Config{})

	tests := map[string]string{
		"alarms":                alarms(3),
		"a long property value": eventWith("DESCRIPTION:" + strings.Repeat("a", hostileParamBytes)),
		"a BEGIN-like value":    eventWith("SUMMARY:BEGIN:X"),
		"a name starting BEGIN": eventWith("X-BEGINNING:soon"),
		"UTF-8 text":            eventWith("SUMMARY:Café ☕"),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if w := put(h, "/alice/work/real.ics", body, nil); w.Code != http.StatusCreated && w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want a stored item\n%s", w.Code, w.Body.String())
			}
		})
	}
}

// refusesContent stands for a backend whose own validation is stricter than
// the library's.
type refusesContent struct{ *caldavmem.Store }

func (refusesContent) CompareAndStoreItem(context.Context, caldav.ItemRef, caldav.StoreItemRequest) (caldav.StoreItemResult, error) {
	return caldav.StoreItemResult{}, &caldav.InvalidContentError{Err: errors.New("the backend's own rule")}
}

func TestPutAnswersABackendContentRefusalAsInvalidData(t *testing.T) {
	h := handlerFor(t, refusesContent{newStore(t)}, caldav.Config{})

	w := put(h, "/alice/work/review.ics", reviewICS, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusForbidden, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "valid-calendar-data") {
		t.Errorf("body = %q, want the CALDAV:valid-calendar-data precondition", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "the backend's own rule") {
		t.Errorf("body = %q leaks the backend's reason", w.Body.String())
	}
}
