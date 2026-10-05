package caldav_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mniehe/davkit/caldav"
	"github.com/mniehe/davkit/caldavmem"
)

const (
	overlapICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:overlap\r\nDTSTAMP:20260801T000000Z\r\n" +
		"DTSTART:20260810T093000Z\r\nDTEND:20260810T110000Z\r\nSUMMARY:Overlap\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	transparentICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:transparent\r\nDTSTAMP:20260801T000000Z\r\n" +
		"DTSTART:20260812T090000Z\r\nDTEND:20260812T100000Z\r\nTRANSP:TRANSPARENT\r\nSUMMARY:OOO\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	cancelledICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:cancelled\r\nDTSTAMP:20260801T000000Z\r\n" +
		"DTSTART:20260813T090000Z\r\nDTEND:20260813T100000Z\r\nSTATUS:CANCELLED\r\nSUMMARY:Off\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	tentativeICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:tentative\r\nDTSTAMP:20260801T000000Z\r\n" +
		"DTSTART:20260814T090000Z\r\nDTEND:20260814T100000Z\r\nSTATUS:TENTATIVE\r\nSUMMARY:Maybe\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	storedFreeBusyICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VFREEBUSY\r\nUID:blocks\r\nDTSTAMP:20260801T000000Z\r\n" +
		"FREEBUSY:20260818T090000Z/20260818T100000Z\r\nFREEBUSY;FBTYPE=FREE:20260819T090000Z/20260819T100000Z\r\nEND:VFREEBUSY\r\nEND:VCALENDAR\r\n"
)

func freeBusyQuery(start, end string) string {
	return `<?xml version="1.0"?>
<C:free-busy-query xmlns:C="urn:ietf:params:xml:ns:caldav">
  <C:time-range start="` + start + `" end="` + end + `"/>
</C:free-busy-query>`
}

func seedKind(t *testing.T, store *caldavmem.Store, account caldav.AccountID, name, ics, uid string, kind caldav.ItemKind) {
	t.Helper()

	ref := caldav.ItemRef{
		Calendar: caldav.CalendarRef{Account: account, Calendar: caldav.MustSegment("work")},
		Item:     caldav.MustSegment(name),
	}
	req := caldav.StoreItemRequest{Content: []byte(ics), ContentID: uid, Kind: kind, MayCreate: true}
	if _, err := store.CompareAndStoreItem(context.Background(), ref, req); err != nil {
		t.Fatalf("seeding %s: %v", name, err)
	}
}

func TestFreeBusyAggregatesBusyTime(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	seedRaw(t, store, "alice", "october.ics", octoberICS, "october")
	h := handlerFor(t, store, caldav.Config{})

	w := report(t, h, "/alice/work/", freeBusyQuery("20260801T000000Z", "20260901T000000Z"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusOK, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/calendar") {
		t.Errorf("Content-Type = %q, want text/calendar", ct)
	}
	body := w.Body.String()
	for _, want := range []string{
		"BEGIN:VFREEBUSY",
		"DTSTART:20260801T000000Z",
		"DTEND:20260901T000000Z",
		"FREEBUSY:20260810T090000Z/20260810T100000Z",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "FREEBUSY:202610") {
		t.Errorf("an event outside the range contributed busy time:\n%s", body)
	}
}

func TestFreeBusyExpandsRecurringEvents(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "weekly.ics", weeklyICS, "weekly")
	h := handlerFor(t, store, caldav.Config{})

	w := report(t, h, "/alice/work/", freeBusyQuery("20260801T000000Z", "20260815T000000Z"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"20260803T090000Z/20260803T093000Z",
		"20260810T090000Z/20260810T093000Z",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing occurrence %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "20260817") {
		t.Errorf("an occurrence outside the range contributed busy time:\n%s", body)
	}
}

func TestFreeBusyCoalescesOverlappingPeriods(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	seedRaw(t, store, "alice", "overlap.ics", overlapICS, "overlap")
	h := handlerFor(t, store, caldav.Config{})

	w := report(t, h, "/alice/work/", freeBusyQuery("20260801T000000Z", "20260901T000000Z"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "FREEBUSY:20260810T090000Z/20260810T110000Z") {
		t.Errorf("body = %q, want the two overlapping events merged into one period", body)
	}
	if got := strings.Count(body, "FREEBUSY:"); got != 1 {
		t.Errorf("body = %q, want exactly one FREEBUSY property, got %d", body, got)
	}
}

func TestFreeBusyClipsPeriodsToTheRange(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, store, caldav.Config{})

	w := report(t, h, "/alice/work/", freeBusyQuery("20260810T093000Z", "20260810T094500Z"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusOK, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "FREEBUSY:20260810T093000Z/20260810T094500Z") {
		t.Errorf("body = %q, want the event clipped to the requested range", body)
	}
}

func TestFreeBusyIgnoresTransparentAndCancelledEvents(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "transparent.ics", transparentICS, "transparent")
	seedRaw(t, store, "alice", "cancelled.ics", cancelledICS, "cancelled")
	h := handlerFor(t, store, caldav.Config{})

	w := report(t, h, "/alice/work/", freeBusyQuery("20260801T000000Z", "20260901T000000Z"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "FREEBUSY:") || strings.Contains(body, "FREEBUSY;") {
		t.Errorf("body = %q, want no busy periods from transparent or cancelled events", body)
	}
}

func TestFreeBusyMarksTentativeTime(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "tentative.ics", tentativeICS, "tentative")
	h := handlerFor(t, store, caldav.Config{})

	w := report(t, h, "/alice/work/", freeBusyQuery("20260801T000000Z", "20260901T000000Z"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusOK, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "FREEBUSY;FBTYPE=BUSY-TENTATIVE:20260814T090000Z/20260814T100000Z") {
		t.Errorf("body = %q, want the tentative event reported as BUSY-TENTATIVE", body)
	}
}

func TestFreeBusyIncludesStoredFreeBusyComponents(t *testing.T) {
	store := newStore(t)
	seedKind(t, store, "alice", "blocks.ics", storedFreeBusyICS, "blocks", caldav.Availability)
	h := handlerFor(t, store, caldav.Config{})

	w := report(t, h, "/alice/work/", freeBusyQuery("20260801T000000Z", "20260901T000000Z"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "FREEBUSY:20260818T090000Z/20260818T100000Z") {
		t.Errorf("body = %q, want the stored busy period", body)
	}
	if strings.Contains(body, "20260819") {
		t.Errorf("body = %q, an FBTYPE=FREE period was reported as busy", body)
	}
}

func TestFreeBusyNeedsOnlyViewAvailability(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, availabilityOnly{store}, caldav.Config{})

	w := report(t, h, "/alice/work/", freeBusyQuery("20260801T000000Z", "20260901T000000Z"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "FREEBUSY:20260810T090000Z/20260810T100000Z") {
		t.Errorf("body = %q, want the busy period a free-busy sharee is entitled to", body)
	}
	for _, leak := range []string{"SUMMARY", "august"} {
		if strings.Contains(body, leak) {
			t.Errorf("body = %q leaks %q to an actor allowed to see only busy times", body, leak)
		}
	}
}

// blindWriter may add items but see nothing, so even busy times are out.
type blindWriter struct{ *caldavmem.Store }

func (blindWriter) CalendarPermissions(context.Context, caldav.Actor, caldav.CalendarRef) (caldav.CalendarPermissions, error) {
	return caldav.CalendarPermissions{CreateItems: true}, nil
}

func TestFreeBusyRequiresViewAvailability(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, blindWriter{store}, caldav.Config{})

	w := report(t, h, "/alice/work/", freeBusyQuery("20260801T000000Z", "20260901T000000Z"))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d\n%s", w.Code, http.StatusForbidden, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "20260810") {
		t.Error("busy times reached an actor with no view permission at all")
	}
}

func TestFreeBusyRequiresAWellFormedTimeRange(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, store, caldav.Config{})

	for name, body := range map[string]string{
		"missing":  `<?xml version="1.0"?><C:free-busy-query xmlns:C="urn:ietf:params:xml:ns:caldav"/>`,
		"empty":    freeBusyQuery("20260801T000000Z", "20260801T000000Z"),
		"reversed": freeBusyQuery("20260901T000000Z", "20260801T000000Z"),
	} {
		t.Run(name, func(t *testing.T) {
			if w := report(t, h, "/alice/work/", body); w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d\n%s", w.Code, http.StatusBadRequest, w.Body.String())
			}
		})
	}
}
