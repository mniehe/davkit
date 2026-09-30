package caldav_test

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mniehe/davkit/caldav"
	"github.com/mniehe/davkit/caldavmem"
)

const (
	augustICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:august\r\nDTSTAMP:20260801T000000Z\r\n" +
		"DTSTART:20260810T090000Z\r\nDTEND:20260810T100000Z\r\nSUMMARY;LANGUAGE=en:August\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	octoberICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:october\r\nDTSTAMP:20260801T000000Z\r\n" +
		"DTSTART:20261010T090000Z\r\nDTEND:20261010T100000Z\r\nSUMMARY:October\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	weeklyICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:weekly\r\nDTSTAMP:20260801T000000Z\r\n" +
		"DTSTART:20260803T090000Z\r\nDTEND:20260803T093000Z\r\nRRULE:FREQ=WEEKLY\r\nSUMMARY:Weekly\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
)

func seedRaw(t *testing.T, store *caldavmem.Store, account caldav.AccountID, name, ics, uid string) {
	t.Helper()

	ref := caldav.ItemRef{
		Calendar: caldav.CalendarRef{Account: account, Calendar: caldav.MustSegment("work")},
		Item:     caldav.MustSegment(name),
	}
	req := caldav.StoreItemRequest{Content: []byte(ics), ContentID: uid, Kind: caldav.Event, MayCreate: true}
	if _, err := store.CompareAndStoreItem(context.Background(), ref, req); err != nil {
		t.Fatalf("seeding %s: %v", name, err)
	}
}

func report(t *testing.T, h *caldav.Handler, target, body string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest("REPORT", target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func reportMS(t *testing.T, h *caldav.Handler, target, body string) multistatus {
	t.Helper()

	w := report(t, h, target, body)
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("REPORT %s: status = %d, want %d\n%s", target, w.Code, http.StatusMultiStatus, w.Body.String())
	}
	var ms multistatus
	if err := xml.Unmarshal(w.Body.Bytes(), &ms); err != nil {
		t.Fatalf("decoding multistatus: %v\n%s", err, w.Body.String())
	}
	return ms
}

func timeRangeQuery(start, end string) string {
	return `<?xml version="1.0"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/><C:calendar-data/></D:prop>
  <C:filter>
    <C:comp-filter name="VCALENDAR">
      <C:comp-filter name="VEVENT">
        <C:time-range start="` + start + `" end="` + end + `"/>
      </C:comp-filter>
    </C:comp-filter>
  </C:filter>
</C:calendar-query>`
}

func TestQueryMatchesByTimeRange(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	seedRaw(t, store, "alice", "october.ics", octoberICS, "october")
	h := handlerFor(t, store, caldav.Config{})

	ms := reportMS(t, h, "/alice/work/", timeRangeQuery("20260801T000000Z", "20260901T000000Z"))

	if got := ms.hrefs(); len(got) != 1 || got[0] != "/alice/work/august.ics" {
		t.Fatalf("hrefs = %v, want only the August event", got)
	}
	resp := ms.at(t, "/alice/work/august.ics")
	if got := resp.value(t, davName("getetag")); got == "" {
		t.Error("no getetag on a query result")
	}
	if got := resp.value(t, caldavName("calendar-data")); !strings.Contains(got, "UID:august") {
		t.Errorf("calendar-data = %q, want the stored event", got)
	}
}

func TestQueryWithNoMatchesIsAnEmptyMultistatus(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, store, caldav.Config{})

	ms := reportMS(t, h, "/alice/work/", timeRangeQuery("20270101T000000Z", "20270201T000000Z"))
	if len(ms.Responses) != 0 {
		t.Errorf("hrefs = %v, want none", ms.hrefs())
	}
}

func TestQueryExpandsRecurrencesInResponse(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "weekly.ics", weeklyICS, "weekly")
	h := handlerFor(t, store, caldav.Config{})

	body := `<?xml version="1.0"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop>
    <C:calendar-data>
      <C:expand start="20260801T000000Z" end="20260822T000000Z"/>
    </C:calendar-data>
  </D:prop>
  <C:filter>
    <C:comp-filter name="VCALENDAR">
      <C:comp-filter name="VEVENT">
        <C:time-range start="20260801T000000Z" end="20260822T000000Z"/>
      </C:comp-filter>
    </C:comp-filter>
  </C:filter>
</C:calendar-query>`

	data := reportMS(t, h, "/alice/work/", body).
		at(t, "/alice/work/weekly.ics").value(t, caldavName("calendar-data"))

	// Three Mondays fall inside the window, so expansion replaces the rule with
	// three instances.
	if got := strings.Count(data, "BEGIN:VEVENT"); got != 3 {
		t.Errorf("expanded to %d instances, want 3:\n%s", got, data)
	}
	if strings.Contains(data, "RRULE") {
		t.Error("an expanded response must not carry the recurrence rule")
	}
}

func TestQueryExpansionDoesNotCorruptTheStoredItem(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "weekly.ics", weeklyICS, "weekly")
	h := handlerFor(t, store, caldav.Config{})

	body := `<?xml version="1.0"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop>
    <C:calendar-data>
      <C:expand start="20260801T000000Z" end="20260822T000000Z"/>
    </C:calendar-data>
  </D:prop>
  <C:filter><C:comp-filter name="VCALENDAR"/></C:filter>
</C:calendar-query>`
	reportMS(t, h, "/alice/work/", body)

	// The expansion worked on the only parsed copy there is. A GET afterwards
	// must still serve the recurrence rule, not the flattened instances.
	if got := do(h, http.MethodGet, "/alice/work/weekly.ics").Body.String(); got != weeklyICS {
		t.Errorf("stored bytes changed after an expanding query:\n%q", got)
	}
}

func TestQueryProjectsRequestedProperties(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, store, caldav.Config{})

	body := `<?xml version="1.0"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop>
    <C:calendar-data>
      <C:comp name="VCALENDAR">
        <C:comp name="VEVENT"><C:prop name="UID"/></C:comp>
      </C:comp>
    </C:calendar-data>
  </D:prop>
  <C:filter><C:comp-filter name="VCALENDAR"/></C:filter>
</C:calendar-query>`

	data := reportMS(t, h, "/alice/work/", body).
		at(t, "/alice/work/august.ics").value(t, caldavName("calendar-data"))

	if !strings.Contains(data, "UID:august") {
		t.Errorf("calendar-data = %q, want the requested UID", data)
	}
	if strings.Contains(data, "SUMMARY") {
		t.Errorf("calendar-data = %q, includes a property the projection excluded", data)
	}
}

// overriddenWeeklyICS is a weekly Monday event with three overridden instances:
// the 10 Aug occurrence moved within August, the 17 Aug occurrence moved out to
// December, and the 5 Oct occurrence moved within October.
const overriddenWeeklyICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:weekly\r\nDTSTAMP:20260801T000000Z\r\n" +
	"DTSTART:20260803T090000Z\r\nDTEND:20260803T093000Z\r\nRRULE:FREQ=WEEKLY\r\nSUMMARY:Weekly\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:weekly\r\nRECURRENCE-ID:20260810T090000Z\r\nDTSTAMP:20260801T000000Z\r\n" +
	"DTSTART:20260810T110000Z\r\nDTEND:20260810T113000Z\r\nSUMMARY:MovedWithinAugust\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:weekly\r\nRECURRENCE-ID:20260817T090000Z\r\nDTSTAMP:20260801T000000Z\r\n" +
	"DTSTART:20261201T090000Z\r\nDTEND:20261201T093000Z\r\nSUMMARY:MovedToDecember\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:weekly\r\nRECURRENCE-ID:20261005T090000Z\r\nDTSTAMP:20260801T000000Z\r\n" +
	"DTSTART:20261005T110000Z\r\nDTEND:20261005T113000Z\r\nSUMMARY:MovedWithinOctober\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func limitRecurrenceQuery(start, end string) string {
	return `<?xml version="1.0"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop>
    <C:calendar-data>
      <C:limit-recurrence-set start="` + start + `" end="` + end + `"/>
    </C:calendar-data>
  </D:prop>
  <C:filter><C:comp-filter name="VCALENDAR"/></C:filter>
</C:calendar-query>`
}

func TestQueryLimitRecurrenceSetKeepsOnlyImpactingOverrides(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "weekly.ics", overriddenWeeklyICS, "weekly")
	h := handlerFor(t, store, caldav.Config{})

	// RFC 4791 §9.6.6: the master is always returned; an overridden component
	// only when its current or original time impacts the window.
	data := reportMS(t, h, "/alice/work/", limitRecurrenceQuery("20260801T000000Z", "20260901T000000Z")).
		at(t, "/alice/work/weekly.ics").value(t, caldavName("calendar-data"))

	if !strings.Contains(data, "RRULE") {
		t.Errorf("calendar-data = %q, limit-recurrence-set must keep the master unexpanded", data)
	}
	if !strings.Contains(data, "MovedWithinAugust") {
		t.Errorf("calendar-data = %q, want the override currently inside the window", data)
	}
	if !strings.Contains(data, "MovedToDecember") {
		t.Errorf("calendar-data = %q, want the override whose original time was inside the window", data)
	}
	if strings.Contains(data, "MovedWithinOctober") {
		t.Errorf("calendar-data = %q, an override impacting the window in neither its current nor original time must be dropped", data)
	}
}

func TestQueryLimitRecurrenceSetExcludesExpand(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "weekly.ics", overriddenWeeklyICS, "weekly")
	h := handlerFor(t, store, caldav.Config{})

	// RFC 4791 §9.6: calendar-data allows (expand | limit-recurrence-set)?, not
	// both.
	body := `<?xml version="1.0"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop>
    <C:calendar-data>
      <C:expand start="20260801T000000Z" end="20260901T000000Z"/>
      <C:limit-recurrence-set start="20260801T000000Z" end="20260901T000000Z"/>
    </C:calendar-data>
  </D:prop>
  <C:filter><C:comp-filter name="VCALENDAR"/></C:filter>
</C:calendar-query>`

	if w := report(t, h, "/alice/work/", body); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for expand combined with limit-recurrence-set", w.Code, http.StatusBadRequest)
	}
}

func TestQueryLimitRecurrenceSetRequiresAnOrderedWindow(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "weekly.ics", overriddenWeeklyICS, "weekly")
	h := handlerFor(t, store, caldav.Config{})

	if w := report(t, h, "/alice/work/", limitRecurrenceQuery("20260901T000000Z", "20260801T000000Z")); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for an inverted window", w.Code, http.StatusBadRequest)
	}
}

func TestQueryLimitFreeBusySetDropsPeriodsOutsideTheWindow(t *testing.T) {
	store := newStore(t)
	busyICS := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VFREEBUSY\r\nUID:busy\r\nDTSTAMP:20260801T000000Z\r\n" +
		"DTSTART:20260801T000000Z\r\nDTEND:20261101T000000Z\r\n" +
		"FREEBUSY:20260810T090000Z/20260810T100000Z,20261010T090000Z/20261010T100000Z\r\n" +
		"FREEBUSY;FBTYPE=BUSY-TENTATIVE:20261015T090000Z/20261015T100000Z\r\nEND:VFREEBUSY\r\nEND:VCALENDAR\r\n"
	ref := caldav.ItemRef{
		Calendar: caldav.CalendarRef{Account: "alice", Calendar: caldav.MustSegment("work")},
		Item:     caldav.MustSegment("busy.ics"),
	}
	req := caldav.StoreItemRequest{Content: []byte(busyICS), ContentID: "busy", Kind: caldav.Availability, MayCreate: true}
	if _, err := store.CompareAndStoreItem(context.Background(), ref, req); err != nil {
		t.Fatalf("seeding busy.ics: %v", err)
	}
	h := handlerFor(t, store, caldav.Config{})

	// RFC 4791 §9.6.7: only the FREEBUSY values intersecting the window are
	// returned; a FREEBUSY left without any values is dropped whole.
	body := limitFreeBusyQuery("20260801T000000Z", "20260901T000000Z")

	data := reportMS(t, h, "/alice/work/", body).
		at(t, "/alice/work/busy.ics").value(t, caldavName("calendar-data"))

	if !strings.Contains(data, "20260810T090000Z/20260810T100000Z") {
		t.Errorf("calendar-data = %q, want the August period kept", data)
	}
	if strings.Contains(data, "20261010T090000Z") {
		t.Errorf("calendar-data = %q, an October period leaked past the window", data)
	}
	if strings.Contains(data, "BUSY-TENTATIVE") {
		t.Errorf("calendar-data = %q, a FREEBUSY whose every value is outside the window must be dropped", data)
	}
}

func TestQueryNoValueStripsTheRequestedValue(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, store, caldav.Config{})

	// RFC 4791 §9.6.4: novalue="yes" asks for the property name and parameters
	// with a trailing ":" and no value data.
	body := `<?xml version="1.0"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop>
    <C:calendar-data>
      <C:comp name="VCALENDAR">
        <C:comp name="VEVENT">
          <C:prop name="UID"/>
          <C:prop name="SUMMARY" novalue="yes"/>
        </C:comp>
      </C:comp>
    </C:calendar-data>
  </D:prop>
  <C:filter><C:comp-filter name="VCALENDAR"/></C:filter>
</C:calendar-query>`

	data := reportMS(t, h, "/alice/work/", body).
		at(t, "/alice/work/august.ics").value(t, caldavName("calendar-data"))

	if !strings.Contains(data, "UID:august") {
		t.Errorf("calendar-data = %q, want the requested UID with its value", data)
	}
	// The RFC keeps the parameters too, so a client learns the property was
	// there and in what form without being sent its contents.
	if !strings.Contains(data, "SUMMARY;LANGUAGE=en:") {
		t.Errorf("calendar-data = %q, want the SUMMARY name and its parameters to survive novalue", data)
	}
	if strings.Contains(data, "=en:August") {
		t.Errorf("calendar-data = %q, novalue=yes must strip the value data", data)
	}
}

func TestQueryNoValueDoesNotCorruptTheStoredItem(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, store, caldav.Config{})

	body := `<?xml version="1.0"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop>
    <C:calendar-data>
      <C:comp name="VCALENDAR">
        <C:comp name="VEVENT"><C:prop name="SUMMARY" novalue="yes"/></C:comp>
      </C:comp>
    </C:calendar-data>
  </D:prop>
  <C:filter><C:comp-filter name="VCALENDAR"/></C:filter>
</C:calendar-query>`
	reportMS(t, h, "/alice/work/", body)

	// Blanking must happen on a copy: a Backend commonly hands out pointers
	// into its own cache.
	if got := do(h, http.MethodGet, "/alice/work/august.ics").Body.String(); got != augustICS {
		t.Errorf("stored bytes changed after a novalue query:\n%q", got)
	}
}

func TestQueryNoValueRejectsAnUnknownValue(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, store, caldav.Config{})

	body := `<?xml version="1.0"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop>
    <C:calendar-data>
      <C:comp name="VCALENDAR">
        <C:comp name="VEVENT"><C:prop name="SUMMARY" novalue="maybe"/></C:comp>
      </C:comp>
    </C:calendar-data>
  </D:prop>
  <C:filter><C:comp-filter name="VCALENDAR"/></C:filter>
</C:calendar-query>`

	if w := report(t, h, "/alice/work/", body); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for a novalue that is neither yes nor no", w.Code, http.StatusBadRequest)
	}
}

func TestQueryBoundsTheResultCount(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	seedRaw(t, store, "alice", "october.ics", octoberICS, "october")
	h := handlerFor(t, store, caldav.Config{MaxSearchResults: 1})

	w := report(t, h, "/alice/work/", timeRangeQuery("20260101T000000Z", "20270101T000000Z"))
	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want %d: a partial answer that looks complete is worse than a refusal", w.Code, http.StatusInsufficientStorage)
	}
	if !strings.Contains(w.Body.String(), "number-of-matches-within-limits") {
		t.Errorf("body = %q, want the DAV:number-of-matches-within-limits precondition", w.Body.String())
	}
}

func TestQueryFailsLoudlyOnUnparseableStoredContent(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "junk.ics", "not icalendar at all", "junk")
	h := handlerFor(t, store, caldav.Config{})

	// A read-only backend can hold bytes the library never validated. Matching
	// cannot be done on them, and silently skipping the item would report a
	// search as complete while omitting a member.
	w := report(t, h, "/alice/work/", timeRangeQuery("20260101T000000Z", "20270101T000000Z"))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

func TestQueryRequiresViewDetails(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, availabilityOnly{store}, caldav.Config{})

	w := report(t, h, "/alice/work/", timeRangeQuery("20260101T000000Z", "20270101T000000Z"))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if strings.Contains(w.Body.String(), "august") {
		t.Error("an item reached an actor allowed to see only busy times")
	}
}

func multigetBody(hrefs ...string) string {
	body := `<?xml version="1.0"?>
<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/><C:calendar-data/></D:prop>`
	for _, href := range hrefs {
		body += "<D:href>" + href + "</D:href>"
	}
	return body + `</C:calendar-multiget>`
}

func TestMultigetReturnsNamedItemsAndMissesAlike(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, store, caldav.Config{})

	ms := reportMS(t, h, "/alice/work/", multigetBody("/alice/work/august.ics", "/alice/work/gone.ics"))

	found := ms.at(t, "/alice/work/august.ics")
	if got := found.value(t, caldavName("calendar-data")); !strings.Contains(got, "UID:august") {
		t.Errorf("calendar-data = %q, want the stored event", got)
	}
	missing := ms.at(t, "/alice/work/gone.ics")
	if missing.Status == "" || !strings.Contains(missing.Status, "404") {
		t.Errorf("missing href status = %q, want a 404 row", missing.Status)
	}
}

func TestMultigetConfinesHrefsToTheCollection(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "carol", "secret.ics", augustICS, "august")
	h := handlerFor(t, store, caldav.Config{})

	// RFC 4791 §7.9: hrefs name members of the request collection. An href into
	// another account's calendar must be refused per-row, never fetched.
	ms := reportMS(t, h, "/alice/work/", multigetBody("/carol/work/secret.ics", "/alice/../carol/work/secret.ics"))

	for _, resp := range ms.Responses {
		if resp.Status == "" || !strings.Contains(resp.Status, "403") {
			t.Errorf("%s: status = %q, want a 403 row", resp.Href, resp.Status)
		}
	}
	if body := multigetBody(); strings.Contains(body, "secret") {
		t.Fatal("fixture error")
	}
}

func TestSyncCollectionInitialSync(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, store, caldav.Config{})

	ms := reportMS(t, h, "/alice/work/", syncBody(""))

	if !contains(ms.hrefs(), "/alice/work/august.ics") {
		t.Fatalf("hrefs = %v, want the seeded item", ms.hrefs())
	}
	if ms.SyncToken == "" {
		t.Fatal("no sync token, so the client has no position to resume from")
	}
}

func TestSyncCollectionDepthOneMatchesDepthZero(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	seedRaw(t, store, "alice", "october.ics", octoberICS, "october")
	h := handlerFor(t, store, caldav.Config{})

	syncAtDepth := func(depth, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("REPORT", "/alice/work/", strings.NewReader(syncBody(token)))
		r.Header.Set("Content-Type", "application/xml")
		r.Header.Set("Depth", depth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	zero := syncAtDepth("0", "")
	one := syncAtDepth("1", "") // Compatibility with clients sending a draft-era Depth: 1.
	if one.Code != http.StatusMultiStatus || zero.Code != one.Code {
		t.Fatalf("Depth 0 status = %d, Depth 1 status = %d, want 207: %s", zero.Code, one.Code, one.Body.String())
	}
	// caldavmem iterates a map, so row order is not stable across requests.
	var ms multistatus
	if err := xml.Unmarshal(one.Body.Bytes(), &ms); err != nil {
		t.Fatalf("decoding Depth 1 response: %v", err)
	}
	var zeroMS multistatus
	if err := xml.Unmarshal(zero.Body.Bytes(), &zeroMS); err != nil {
		t.Fatalf("decoding Depth 0 response: %v", err)
	}
	if got := ms.hrefs(); len(got) != 2 || !contains(got, "/alice/work/august.ics") || !contains(got, "/alice/work/october.ics") {
		t.Errorf("initial hrefs = %v, want both objects", got)
	}
	token := ms.SyncToken
	if token == "" {
		t.Fatal("initial sync returned no token")
	}
	if zeroMS.SyncToken != token || len(zeroMS.Responses) != len(ms.Responses) {
		t.Fatalf("initial responses differ by Depth: 0 = %s; 1 = %s", zero.Body.String(), one.Body.String())
	}
	for _, row := range ms.Responses {
		if !reflect.DeepEqual(row, zeroMS.at(t, row.Href)) {
			t.Errorf("initial row %s differs by Depth", row.Href)
		}
	}

	seedRaw(t, store, "alice", "weekly.ics", weeklyICS, "weekly")
	zero = syncAtDepth("0", token)
	one = syncAtDepth("1", token)
	if one.Code != http.StatusMultiStatus || zero.Code != one.Code {
		t.Fatalf("delta differs by Depth: 0 = %d %s; 1 = %d %s", zero.Code, zero.Body.String(), one.Code, one.Body.String())
	}
	ms = multistatus{}
	if err := xml.Unmarshal(one.Body.Bytes(), &ms); err != nil {
		t.Fatalf("decoding Depth 1 delta: %v", err)
	}
	zeroMS = multistatus{}
	if err := xml.Unmarshal(zero.Body.Bytes(), &zeroMS); err != nil {
		t.Fatalf("decoding Depth 0 delta: %v", err)
	}
	if zeroMS.SyncToken != ms.SyncToken || len(zeroMS.Responses) != len(ms.Responses) || !reflect.DeepEqual(zeroMS.at(t, "/alice/work/weekly.ics"), ms.at(t, "/alice/work/weekly.ics")) {
		t.Errorf("delta response differs by Depth: 0 = %s; 1 = %s", zero.Body.String(), one.Body.String())
	}
	if got := ms.hrefs(); len(got) != 1 || got[0] != "/alice/work/weekly.ics" {
		t.Errorf("delta hrefs = %v, want only the changed object", got)
	}
	if ms.SyncToken == "" || ms.SyncToken == token {
		t.Errorf("token did not advance: %q", ms.SyncToken)
	}
}

func TestSyncCollectionDepthOneStillRefusesInvalidDepthAndSyncLevel(t *testing.T) {
	h := handlerFor(t, newStore(t), caldav.Config{})
	for _, depth := range []string{"infinity", "2", "banana"} {
		t.Run(depth, func(t *testing.T) {
			r := httptest.NewRequest("REPORT", "/alice/work/", strings.NewReader(syncBody("")))
			r.Header.Set("Content-Type", "application/xml")
			r.Header.Set("Depth", depth)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("Depth %q: status = %d, want 400", depth, w.Code)
			}
		})
	}
	for _, level := range []string{"", "infinite", "banana"} {
		t.Run("level "+level, func(t *testing.T) {
			r := httptest.NewRequest("REPORT", "/alice/work/", strings.NewReader(strings.Replace(syncBody(""), "<D:sync-level>1</D:sync-level>", "<D:sync-level>"+level+"</D:sync-level>", 1)))
			r.Header.Set("Content-Type", "application/xml")
			r.Header.Set("Depth", "1")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("sync-level %q: status = %d, want 400", level, w.Code)
			}
		})
	}
	t.Run("missing sync-level", func(t *testing.T) {
		r := httptest.NewRequest("REPORT", "/alice/work/", strings.NewReader(strings.Replace(syncBody(""), "<D:sync-level>1</D:sync-level>", "", 1)))
		r.Header.Set("Content-Type", "application/xml")
		r.Header.Set("Depth", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("missing sync-level: status = %d, want 400", w.Code)
		}
	})
	if w := report(t, h, "/alice/work/", syncBody("")); w.Code != http.StatusMultiStatus {
		t.Errorf("absent Depth: status = %d, want 207", w.Code)
	}
}

func TestSyncCollectionReportsTheDelta(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, store, caldav.Config{})

	token := reportMS(t, h, "/alice/work/", syncBody("")).SyncToken

	seedRaw(t, store, "alice", "october.ics", octoberICS, "october")
	ms := reportMS(t, h, "/alice/work/", syncBody(token))

	if got := ms.hrefs(); len(got) != 1 || got[0] != "/alice/work/october.ics" {
		t.Errorf("hrefs = %v, want only the item added since the token", got)
	}
	if ms.SyncToken == "" || ms.SyncToken == token {
		t.Errorf("token did not advance: %q", ms.SyncToken)
	}
}

func TestSyncCollectionReportsDeletionsAsNotFoundRows(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, store, caldav.Config{})
	token := reportMS(t, h, "/alice/work/", syncBody("")).SyncToken

	if w := del(h, "/alice/work/august.ics", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}

	ms := reportMS(t, h, "/alice/work/", syncBody(token))
	row := ms.at(t, "/alice/work/august.ics")
	if row.Status == "" || !strings.Contains(row.Status, "404") {
		t.Errorf("deleted item status = %q, want a 404 row telling the client to drop it", row.Status)
	}
}

func TestSyncCollectionRefusesAForeignToken(t *testing.T) {
	store := newStore(t)
	h := handlerFor(t, store, caldav.Config{})

	// RFC 6578 §3.2: an unserviceable token MUST be DAV:valid-sync-token. A
	// silent full listing carries no deletions, so the client would keep
	// removed items forever.
	for name, token := range map[string]string{
		"garbage":          "not-a-token",
		"another calendar": reportMS(t, handlerFor(t, store, caldav.Config{}), "/alice/work/", syncBody("")).SyncToken + "0",
	} {
		t.Run(name, func(t *testing.T) {
			w := report(t, h, "/alice/work/", syncBody(token))
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
			}
			if !strings.Contains(w.Body.String(), "valid-sync-token") {
				t.Errorf("body = %q, want DAV:valid-sync-token", w.Body.String())
			}
		})
	}
}

func TestSyncCollectionRefusesPrunedHistory(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, store, caldav.Config{})
	token := reportMS(t, h, "/alice/work/", syncBody("")).SyncToken

	seedRaw(t, store, "alice", "october.ics", octoberICS, "october")
	ref := caldav.CalendarRef{Account: "alice", Calendar: caldav.MustSegment("work")}
	if err := store.PruneHistory(context.Background(), ref, 99); err != nil {
		t.Fatalf("pruning: %v", err)
	}

	w := report(t, h, "/alice/work/", syncBody(token))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "valid-sync-token") {
		t.Errorf("status = %d body = %q, want 403 DAV:valid-sync-token", w.Code, w.Body.String())
	}
}

func TestSyncCollectionNeedsASyncingBackend(t *testing.T) {
	store := newStore(t)
	h := handlerFor(t, readOnlyBackend{store}, caldav.Config{})

	if w := report(t, h, "/alice/work/", syncBody("")); w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

func syncBody(token string) string {
	return `<?xml version="1.0"?>
<D:sync-collection xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:sync-token>` + token + `</D:sync-token>
  <D:sync-level>1</D:sync-level>
  <D:prop><D:getetag/></D:prop>
</D:sync-collection>`
}

func TestSupportedReportSetMatchesWhatIsDispatched(t *testing.T) {
	store := newStore(t)
	syncing := handlerFor(t, store, caldav.Config{})
	plain := handlerFor(t, readOnlyBackend{store}, caldav.Config{})

	ask := askFor(davName("supported-report-set"))
	full := propfind(t, syncing, "/alice/work/", "0", ask).at(t, "/alice/work/").value(t, davName("supported-report-set"))
	for _, want := range []string{"calendar-query", "calendar-multiget", "free-busy-query", "sync-collection"} {
		if !strings.Contains(full, want) {
			t.Errorf("syncing backend's report set = %q, missing %s", full, want)
		}
	}

	readonly := propfind(t, plain, "/alice/work/", "0", ask).at(t, "/alice/work/").value(t, davName("supported-report-set"))
	if strings.Contains(readonly, "sync-collection") {
		t.Errorf("report set = %q advertises sync-collection, which this backend answers 501", readonly)
	}
}

func TestReportOnAnAccountIsRefused(t *testing.T) {
	h := handlerFor(t, newStore(t), caldav.Config{})

	w := report(t, h, "/alice/", timeRangeQuery("20260101T000000Z", "20270101T000000Z"))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func contains(hrefs []string, want string) bool {
	for _, h := range hrefs {
		if h == want {
			return true
		}
	}
	return false
}

func expandQuery(start, end string) string {
	return `<?xml version="1.0"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop>
    <C:calendar-data>
      <C:expand start="` + start + `" end="` + end + `"/>
    </C:calendar-data>
  </D:prop>
  <C:filter><C:comp-filter name="VCALENDAR"/></C:filter>
</C:calendar-query>`
}

func limitFreeBusyQuery(start, end string) string {
	return `<?xml version="1.0"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop>
    <C:calendar-data>
      <C:limit-freebusy-set start="` + start + `" end="` + end + `"/>
    </C:calendar-data>
  </D:prop>
  <C:filter><C:comp-filter name="VCALENDAR"/></C:filter>
</C:calendar-query>`
}

func TestQueryFailsLoudlyOnUnreadableShapedContent(t *testing.T) {
	// Shaping reads stored values the library validated on the way in, so a
	// value it cannot read is a contract violation, not a client error. The
	// policy TestQueryFailsLoudlyOnUnparseableStoredContent states applies to
	// every shaping path alike.
	tests := map[string]struct{ ics, body string }{
		"an override with an unreadable interval": {
			"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
				"BEGIN:VEVENT\r\nUID:broken\r\nDTSTAMP:20260801T000000Z\r\nRECURRENCE-ID:20261005T090000Z\r\n" +
				"DTSTART:not-a-time\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
			limitRecurrenceQuery("20260801T000000Z", "20260901T000000Z"),
		},
		"an override with an unreadable RECURRENCE-ID": {
			"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
				"BEGIN:VEVENT\r\nUID:broken\r\nDTSTAMP:20260801T000000Z\r\nRECURRENCE-ID:nonsense\r\n" +
				"DTSTART:20261005T110000Z\r\nDTEND:20261005T113000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
			limitRecurrenceQuery("20260801T000000Z", "20260901T000000Z"),
		},
		"a FREEBUSY that is not a period": {
			"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
				"BEGIN:VFREEBUSY\r\nUID:broken\r\nDTSTAMP:20260801T000000Z\r\nFREEBUSY:not-a-period\r\n" +
				"END:VFREEBUSY\r\nEND:VCALENDAR\r\n",
			limitFreeBusyQuery("20260801T000000Z", "20260901T000000Z"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			store := newStore(t)
			seedRaw(t, store, "alice", "broken.ics", test.ics, "broken")
			h := handlerFor(t, store, caldav.Config{})

			if w := report(t, h, "/alice/work/", test.body); w.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want %d\n%s", w.Code, http.StatusInternalServerError, w.Body.String())
			}
		})
	}
}

func windowQuery(element, attrs string) string {
	return `<?xml version="1.0"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop>
    <C:calendar-data>
      <C:` + element + ` ` + attrs + `/>
    </C:calendar-data>
  </D:prop>
  <C:filter><C:comp-filter name="VCALENDAR"/></C:filter>
</C:calendar-query>`
}

func TestReportWindowsRequireAnOrderedStartAndEnd(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "august.ics", augustICS, "august")
	h := handlerFor(t, store, caldav.Config{})

	// A missing attribute decodes as the zero time, which would otherwise ask
	// for an unbounded window over every stored object.
	windows := map[string]string{
		"missing start": `end="20260901T000000Z"`,
		"missing end":   `start="20260801T000000Z"`,
		"inverted":      `start="20260901T000000Z" end="20260801T000000Z"`,
	}
	for _, element := range []string{"expand", "limit-recurrence-set", "limit-freebusy-set"} {
		for name, attrs := range windows {
			t.Run(element+"/"+name, func(t *testing.T) {
				w := report(t, h, "/alice/work/", windowQuery(element, attrs))
				if w.Code != http.StatusBadRequest {
					t.Errorf("status = %d, want %d\n%s", w.Code, http.StatusBadRequest, w.Body.String())
				}
			})
		}
	}
}
