package carddav_test

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mniehe/davkit/carddav"
	"github.com/mniehe/davkit/carddavmem"
)

const (
	adaVCF = "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:ada\r\nFN:Ada Lovelace\r\n" +
		"EMAIL;TYPE=work:ada@example.com\r\nTEL;TYPE=home:+1-555-0100\r\nEND:VCARD\r\n"
	bobVCF = "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:bob\r\nFN:Bob Babbage\r\n" +
		"EMAIL;TYPE=home:bob@example.org\r\nEND:VCARD\r\n"
	novaV3VCF  = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:nova\r\nFN:Nova Three\r\nEND:VCARD\r\n"
	groupedVCF = "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:grp\r\nFN:Grouped Contact\r\n" +
		"WORK.TEL:+1-555-0199\r\nEND:VCARD\r\n"
)

func seedRaw(t *testing.T, store *carddavmem.Store, account carddav.AccountID, name, vcf, uid string) {
	t.Helper()

	ref := carddav.ItemRef{
		Book: carddav.AddressBookRef{Account: account, Book: carddav.MustSegment("work")},
		Item: carddav.MustSegment(name),
	}
	req := carddav.StoreItemRequest{Content: []byte(vcf), ContentID: uid, MayCreate: true}
	if _, err := store.CompareAndStoreItem(context.Background(), ref, req); err != nil {
		t.Fatalf("seeding %s: %v", name, err)
	}
}

func report(t *testing.T, h *carddav.Handler, target, body string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest("REPORT", target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func reportMS(t *testing.T, h *carddav.Handler, target, body string) multistatus {
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

func filterQuery(filter string) string {
	return `<?xml version="1.0"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop><D:getetag/><C:address-data/></D:prop>
  ` + filter + `
</C:addressbook-query>`
}

func abStore(t *testing.T) *carddavmem.Store {
	t.Helper()

	store := newStore(t)
	seedRaw(t, store, "alice", "ada.vcf", adaVCF, "ada")
	seedRaw(t, store, "alice", "bob.vcf", bobVCF, "bob")
	return store
}

func TestQueryMatchesByTextMatch(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	// The default collation is case-insensitive (RFC 6352 §10.5.4), so the
	// lowercase needle has to find the capitalised name.
	ms := reportMS(t, h, "/alice/work/",
		filterQuery(`<C:filter><C:prop-filter name="FN"><C:text-match>lovelace</C:text-match></C:prop-filter></C:filter>`))

	if got := ms.hrefs(); len(got) != 1 || got[0] != "/alice/work/ada.vcf" {
		t.Fatalf("hrefs = %v, want only Ada", got)
	}
	resp := ms.at(t, "/alice/work/ada.vcf")
	if got := resp.value(t, davName("getetag")); got == "" {
		t.Error("no getetag on a query result")
	}
	if got := resp.value(t, carddavName("address-data")); !strings.Contains(got, "UID:ada") {
		t.Errorf("address-data = %q, want the stored card", got)
	}
}

func TestQueryWithNoMatchesIsAnEmptyMultistatus(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	ms := reportMS(t, h, "/alice/work/",
		filterQuery(`<C:filter><C:prop-filter name="FN"><C:text-match>hopper</C:text-match></C:prop-filter></C:filter>`))
	if len(ms.Responses) != 0 {
		t.Errorf("hrefs = %v, want none", ms.hrefs())
	}
}

func TestQueryFilterTestSelectsAnyOrAll(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	two := `<C:prop-filter name="FN"><C:text-match>lovelace</C:text-match></C:prop-filter>` +
		`<C:prop-filter name="EMAIL"><C:text-match>example.org</C:text-match></C:prop-filter>`

	anyOf := reportMS(t, h, "/alice/work/", filterQuery(`<C:filter test="anyof">`+two+`</C:filter>`))
	if got := anyOf.hrefs(); len(got) != 2 {
		t.Errorf("anyof hrefs = %v, want both cards", got)
	}

	allOf := reportMS(t, h, "/alice/work/", filterQuery(`<C:filter test="allof">`+two+`</C:filter>`))
	if got := allOf.hrefs(); len(got) != 0 {
		t.Errorf("allof hrefs = %v, want none: no card satisfies both", got)
	}

	both := `<C:prop-filter name="FN"><C:text-match>babbage</C:text-match></C:prop-filter>` +
		`<C:prop-filter name="EMAIL"><C:text-match>example.org</C:text-match></C:prop-filter>`
	allBoth := reportMS(t, h, "/alice/work/", filterQuery(`<C:filter test="allof">`+both+`</C:filter>`))
	if got := allBoth.hrefs(); len(got) != 1 || got[0] != "/alice/work/bob.vcf" {
		t.Errorf("allof hrefs = %v, want only Bob", got)
	}
}

func TestPropFilterTestSelectsAnyOrAll(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	// Both needles occur in Ada's FN taken together, but only one at a time:
	// the prop-filter's own test attribute decides whether one suffices.
	two := `<C:text-match>ada</C:text-match><C:text-match>babbage</C:text-match>`

	anyOf := reportMS(t, h, "/alice/work/",
		filterQuery(`<C:filter><C:prop-filter name="FN" test="anyof">`+two+`</C:prop-filter></C:filter>`))
	got := anyOf.hrefs()
	slices.Sort(got)
	if want := []string{"/alice/work/ada.vcf", "/alice/work/bob.vcf"}; !slices.Equal(got, want) {
		t.Errorf("anyof hrefs = %v, want %v", got, want)
	}

	allOf := reportMS(t, h, "/alice/work/",
		filterQuery(`<C:filter><C:prop-filter name="FN" test="allof">`+two+`</C:prop-filter></C:filter>`))
	if got := allOf.hrefs(); len(got) != 0 {
		t.Errorf("allof hrefs = %v, want none: no single FN carries both", got)
	}
}

func TestQueryIsNotDefined(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	ms := reportMS(t, h, "/alice/work/",
		filterQuery(`<C:filter><C:prop-filter name="TEL"><C:is-not-defined/></C:prop-filter></C:filter>`))
	if got := ms.hrefs(); len(got) != 1 || got[0] != "/alice/work/bob.vcf" {
		t.Errorf("hrefs = %v, want only the card without a TEL", got)
	}
}

func TestQueryParamFilter(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	ms := reportMS(t, h, "/alice/work/",
		filterQuery(`<C:filter><C:prop-filter name="EMAIL"><C:param-filter name="TYPE"><C:text-match>work</C:text-match></C:param-filter></C:prop-filter></C:filter>`))
	if got := ms.hrefs(); len(got) != 1 || got[0] != "/alice/work/ada.vcf" {
		t.Errorf("hrefs = %v, want only the work email", got)
	}
}

func TestQueryParamFilterIsNotDefined(t *testing.T) {
	store := abStore(t)
	seedRaw(t, store, "alice", "grp.vcf", groupedVCF, "grp")
	h := handlerFor(t, store, carddav.Config{})

	// Ada's TEL carries TYPE=home; the grouped card's TEL has no TYPE at all.
	ms := reportMS(t, h, "/alice/work/",
		filterQuery(`<C:filter><C:prop-filter name="TEL"><C:param-filter name="TYPE"><C:is-not-defined/></C:param-filter></C:prop-filter></C:filter>`))
	if got := ms.hrefs(); len(got) != 1 || got[0] != "/alice/work/grp.vcf" {
		t.Errorf("hrefs = %v, want only the TEL without a TYPE", got)
	}
}

func TestQueryTextMatchTypes(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	tests := map[string]struct {
		match string
		want  []string
	}{
		"equals": {
			`<C:text-match match-type="equals">ada lovelace</C:text-match>`,
			[]string{"/alice/work/ada.vcf"},
		},
		"starts-with": {
			`<C:text-match match-type="starts-with">bob</C:text-match>`,
			[]string{"/alice/work/bob.vcf"},
		},
		"ends-with": {
			`<C:text-match match-type="ends-with">babbage</C:text-match>`,
			[]string{"/alice/work/bob.vcf"},
		},
		"negated contains": {
			`<C:text-match negate-condition="yes">lovelace</C:text-match>`,
			[]string{"/alice/work/bob.vcf"},
		},
		// The needle occurs inside both names, so any of these matching would
		// mean the match type degraded to a contains.
		"equals is not contains": {
			`<C:text-match match-type="equals">ada</C:text-match>`,
			nil,
		},
		"starts-with is not contains": {
			`<C:text-match match-type="starts-with">lovelace</C:text-match>`,
			nil,
		},
		"ends-with is not contains": {
			`<C:text-match match-type="ends-with">bob</C:text-match>`,
			nil,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ms := reportMS(t, h, "/alice/work/",
				filterQuery(`<C:filter><C:prop-filter name="FN">`+tc.match+`</C:prop-filter></C:filter>`))
			got := ms.hrefs()
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("hrefs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQueryMatchesAGroupedPropertyName(t *testing.T) {
	store := abStore(t)
	seedRaw(t, store, "alice", "grp.vcf", groupedVCF, "grp")
	h := handlerFor(t, store, carddav.Config{})

	// RFC 6352 §10.5.1: a bare name matches the property in any group, while a
	// group-prefixed name selects only that group's occurrence.
	grouped := reportMS(t, h, "/alice/work/",
		filterQuery(`<C:filter><C:prop-filter name="WORK.TEL"/></C:filter>`))
	if got := grouped.hrefs(); len(got) != 1 || got[0] != "/alice/work/grp.vcf" {
		t.Errorf("grouped hrefs = %v, want only the WORK-grouped TEL", got)
	}

	bare := reportMS(t, h, "/alice/work/",
		filterQuery(`<C:filter><C:prop-filter name="TEL"/></C:filter>`))
	got := bare.hrefs()
	slices.Sort(got)
	if want := []string{"/alice/work/ada.vcf", "/alice/work/grp.vcf"}; !slices.Equal(got, want) {
		t.Errorf("bare hrefs = %v, want %v", got, want)
	}
}

func TestQueryRejectsAnUnknownFilterTest(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	w := report(t, h, "/alice/work/",
		filterQuery(`<C:filter test="someof"><C:prop-filter name="FN"/></C:filter>`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestQueryRejectsAContradictoryFilter(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	// is-not-defined asks about absence; pairing it with a condition on the
	// value is contradictory (RFC 6352 §10.5.1).
	tests := map[string]string{
		"prop-filter": `<C:filter><C:prop-filter name="FN"><C:is-not-defined/><C:text-match>x</C:text-match></C:prop-filter></C:filter>`,
		"param-filter": `<C:filter><C:prop-filter name="EMAIL"><C:param-filter name="TYPE">` +
			`<C:is-not-defined/><C:text-match>work</C:text-match></C:param-filter></C:prop-filter></C:filter>`,
	}
	for name, filter := range tests {
		t.Run(name, func(t *testing.T) {
			if w := report(t, h, "/alice/work/", filterQuery(filter)); w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d\n%s", w.Code, http.StatusBadRequest, w.Body.String())
			}
		})
	}
}

func TestQueryProjectsRequestedProperties(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	body := `<?xml version="1.0"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <C:address-data><C:prop name="FN"/></C:address-data>
  </D:prop>
  <C:filter><C:prop-filter name="FN"><C:text-match>lovelace</C:text-match></C:prop-filter></C:filter>
</C:addressbook-query>`

	data := reportMS(t, h, "/alice/work/", body).
		at(t, "/alice/work/ada.vcf").value(t, carddavName("address-data"))

	if !strings.Contains(data, "FN:Ada Lovelace") {
		t.Errorf("address-data = %q, want the requested FN", data)
	}
	// A card without VERSION does not encode, so the projection carries it.
	if !strings.Contains(data, "VERSION:") {
		t.Errorf("address-data = %q, lost the VERSION a vCard cannot omit", data)
	}
	if strings.Contains(data, "EMAIL") {
		t.Errorf("address-data = %q, includes a property the projection excluded", data)
	}
}

func TestQueryNoValueStripsTheRequestedValue(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	// RFC 6352 §10.4.2: novalue="yes" asks for the property name and parameters
	// with the value data stripped.
	body := `<?xml version="1.0"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <C:address-data>
      <C:prop name="FN"/>
      <C:prop name="EMAIL" novalue="yes"/>
    </C:address-data>
  </D:prop>
  <C:filter><C:prop-filter name="FN"><C:text-match>lovelace</C:text-match></C:prop-filter></C:filter>
</C:addressbook-query>`

	data := reportMS(t, h, "/alice/work/", body).
		at(t, "/alice/work/ada.vcf").value(t, carddavName("address-data"))

	if !strings.Contains(data, "FN:Ada Lovelace") {
		t.Errorf("address-data = %q, want the requested FN with its value", data)
	}
	// The RFC keeps the parameters too, so a client learns the property was
	// there and in what form without being sent its contents.
	if !strings.Contains(data, "EMAIL;TYPE=work:") {
		t.Errorf("address-data = %q, want the EMAIL name and its parameters to survive novalue", data)
	}
	if strings.Contains(data, "ada@example.com") {
		t.Errorf("address-data = %q, novalue=yes must strip the value data", data)
	}
}

func TestQueryNoValueDoesNotCorruptTheStoredCard(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	body := `<?xml version="1.0"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <C:address-data><C:prop name="EMAIL" novalue="yes"/></C:address-data>
  </D:prop>
  <C:filter><C:prop-filter name="FN"><C:text-match>lovelace</C:text-match></C:prop-filter></C:filter>
</C:addressbook-query>`
	reportMS(t, h, "/alice/work/", body)

	// Blanking must happen on a copy: the store commonly hands out pointers
	// into its own cache.
	if got := do(h, http.MethodGet, "/alice/work/ada.vcf").Body.String(); got != adaVCF {
		t.Errorf("stored bytes changed after a novalue query:\n%q", got)
	}
}

func TestQueryNoValueRejectsAnUnknownValue(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	body := `<?xml version="1.0"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <C:address-data><C:prop name="EMAIL" novalue="maybe"/></C:address-data>
  </D:prop>
  <C:filter><C:prop-filter name="FN"/></C:filter>
</C:addressbook-query>`

	if w := report(t, h, "/alice/work/", body); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for a novalue that is neither yes nor no", w.Code, http.StatusBadRequest)
	}
}

func TestQueryConvertsToTheRequestedVersion(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "nova.vcf", novaV3VCF, "nova")
	h := handlerFor(t, store, carddav.Config{})

	body := `<?xml version="1.0"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <C:address-data content-type="text/vcard" version="4.0"/>
  </D:prop>
  <C:filter><C:prop-filter name="FN"/></C:filter>
</C:addressbook-query>`

	data := reportMS(t, h, "/alice/work/", body).
		at(t, "/alice/work/nova.vcf").value(t, carddavName("address-data"))
	if !strings.Contains(data, "VERSION:4.0") {
		t.Errorf("address-data = %q, want the card upgraded to 4.0", data)
	}
}

func TestQueryRefusesAnUnsupportedConversion(t *testing.T) {
	// The empty book matters: the refusal must come from validating the
	// request, not from failing to convert some matched card.
	h := handlerFor(t, newStore(t), carddav.Config{})

	body := `<?xml version="1.0"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <C:address-data content-type="text/vcard" version="2.1"/>
  </D:prop>
  <C:filter><C:prop-filter name="FN"/></C:filter>
</C:addressbook-query>`

	w := report(t, h, "/alice/work/", body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusForbidden, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "supported-address-data") {
		t.Errorf("body = %q, want the CARDDAV:supported-address-data precondition", w.Body.String())
	}
}

func TestQueryHonoursNResultsWithATruncationRow(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	body := `<?xml version="1.0"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop><D:getetag/></D:prop>
  <C:filter><C:prop-filter name="FN"/></C:filter>
  <C:limit><C:nresults>1</C:nresults></C:limit>
</C:addressbook-query>`

	w := report(t, h, "/alice/work/", body)
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusMultiStatus, w.Body.String())
	}
	var ms multistatus
	if err := xml.Unmarshal(w.Body.Bytes(), &ms); err != nil {
		t.Fatalf("decoding multistatus: %v", err)
	}
	// One member row plus the 507 marker row RFC 6352 §8.6.2 requires, so the
	// client can tell a full answer from a cut-off one.
	if len(ms.Responses) != 2 {
		t.Fatalf("got %d rows, want a member and the truncation marker:\n%s", len(ms.Responses), w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "number-of-matches-within-limits") {
		t.Errorf("body = %q, want the DAV:number-of-matches-within-limits marker", w.Body.String())
	}
}

func TestQueryBoundsTheResultCount(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{MaxSearchResults: 1})

	w := report(t, h, "/alice/work/", filterQuery(`<C:filter><C:prop-filter name="FN"/></C:filter>`))
	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want %d: a partial answer that looks complete is worse than a refusal", w.Code, http.StatusInsufficientStorage)
	}
	if !strings.Contains(w.Body.String(), "number-of-matches-within-limits") {
		t.Errorf("body = %q, want the DAV:number-of-matches-within-limits precondition", w.Body.String())
	}
}

func TestQueryFailsLoudlyOnUnparseableStoredContent(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "junk.vcf", "not a vcard at all", "junk")
	h := handlerFor(t, store, carddav.Config{})

	// A read-only backend can hold bytes the library never validated. Matching
	// cannot be done on them, and silently skipping the item would report a
	// search as complete while omitting a member.
	w := report(t, h, "/alice/work/", filterQuery(`<C:filter><C:prop-filter name="FN"/></C:filter>`))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// writeOnlyBackend grants item writes but no reading, so a REPORT reaches the
// permission gate rather than being concealed outright.
type writeOnlyBackend struct{ *carddavmem.Store }

func (writeOnlyBackend) AddressBookPermissions(context.Context, carddav.Actor, carddav.AddressBookRef) (carddav.AddressBookPermissions, error) {
	return carddav.AddressBookPermissions{CreateItems: true}, nil
}

func TestQueryRequiresViewDetails(t *testing.T) {
	store := abStore(t)
	h := handlerFor(t, writeOnlyBackend{store}, carddav.Config{})

	w := report(t, h, "/alice/work/", filterQuery(`<C:filter><C:prop-filter name="FN"/></C:filter>`))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if strings.Contains(w.Body.String(), "ada") {
		t.Error("a card reached an actor who may not read this address book")
	}
}

func multigetBody(hrefs ...string) string {
	body := `<?xml version="1.0"?>
<C:addressbook-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop><D:getetag/><C:address-data/></D:prop>`
	for _, href := range hrefs {
		body += "<D:href>" + href + "</D:href>"
	}
	return body + `</C:addressbook-multiget>`
}

func TestMultigetReturnsNamedItemsAndMissesAlike(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	ms := reportMS(t, h, "/alice/work/", multigetBody("/alice/work/ada.vcf", "/alice/work/gone.vcf"))

	found := ms.at(t, "/alice/work/ada.vcf")
	if got := found.value(t, carddavName("address-data")); !strings.Contains(got, "UID:ada") {
		t.Errorf("address-data = %q, want the stored card", got)
	}
	missing := ms.at(t, "/alice/work/gone.vcf")
	if missing.Status == "" || !strings.Contains(missing.Status, "404") {
		t.Errorf("missing href status = %q, want a 404 row", missing.Status)
	}
}

func TestMultigetConfinesHrefsToTheCollection(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "carol", "secret.vcf", adaVCF, "ada")
	h := handlerFor(t, store, carddav.Config{})

	// RFC 6352 §8.7: hrefs name members of the request collection. An href into
	// another account's address book must be refused per-row, never fetched.
	ms := reportMS(t, h, "/alice/work/", multigetBody("/carol/work/secret.vcf", "/alice/../carol/work/secret.vcf"))

	for _, resp := range ms.Responses {
		if resp.Status == "" || !strings.Contains(resp.Status, "403") {
			t.Errorf("%s: status = %q, want a 403 row", resp.Href, resp.Status)
		}
	}
}

func syncBody(token string) string {
	return `<?xml version="1.0"?>
<D:sync-collection xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:sync-token>` + token + `</D:sync-token>
  <D:sync-level>1</D:sync-level>
  <D:prop><D:getetag/></D:prop>
</D:sync-collection>`
}

func TestSyncCollectionInitialSync(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "ada.vcf", adaVCF, "ada")
	h := handlerFor(t, store, carddav.Config{})

	ms := reportMS(t, h, "/alice/work/", syncBody(""))

	if !slices.Contains(ms.hrefs(), "/alice/work/ada.vcf") {
		t.Fatalf("hrefs = %v, want the seeded item", ms.hrefs())
	}
	if ms.SyncToken == "" {
		t.Fatal("no sync token, so the client has no position to resume from")
	}
}

func TestSyncCollectionDepthOneMatchesDepthZero(t *testing.T) {
	store := abStore(t)
	h := handlerFor(t, store, carddav.Config{})

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
	// carddavmem iterates a map, so row order is not stable across requests.
	var ms multistatus
	if err := xml.Unmarshal(one.Body.Bytes(), &ms); err != nil {
		t.Fatalf("decoding Depth 1 response: %v", err)
	}
	var zeroMS multistatus
	if err := xml.Unmarshal(zero.Body.Bytes(), &zeroMS); err != nil {
		t.Fatalf("decoding Depth 0 response: %v", err)
	}
	if got := ms.hrefs(); len(got) != 2 || !slices.Contains(got, "/alice/work/ada.vcf") || !slices.Contains(got, "/alice/work/bob.vcf") {
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

	seedRaw(t, store, "alice", "nova.vcf", novaV3VCF, "nova")
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
	if zeroMS.SyncToken != ms.SyncToken || len(zeroMS.Responses) != len(ms.Responses) || !reflect.DeepEqual(zeroMS.at(t, "/alice/work/nova.vcf"), ms.at(t, "/alice/work/nova.vcf")) {
		t.Errorf("delta response differs by Depth: 0 = %s; 1 = %s", zero.Body.String(), one.Body.String())
	}
	if got := ms.hrefs(); len(got) != 1 || got[0] != "/alice/work/nova.vcf" {
		t.Errorf("delta hrefs = %v, want only the changed object", got)
	}
	if ms.SyncToken == "" || ms.SyncToken == token {
		t.Errorf("token did not advance: %q", ms.SyncToken)
	}
}

func TestSyncCollectionDepthOneStillRefusesInvalidDepthAndSyncLevel(t *testing.T) {
	h := handlerFor(t, newStore(t), carddav.Config{})
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
	seedRaw(t, store, "alice", "ada.vcf", adaVCF, "ada")
	h := handlerFor(t, store, carddav.Config{})

	token := reportMS(t, h, "/alice/work/", syncBody("")).SyncToken

	seedRaw(t, store, "alice", "bob.vcf", bobVCF, "bob")
	ms := reportMS(t, h, "/alice/work/", syncBody(token))

	if got := ms.hrefs(); len(got) != 1 || got[0] != "/alice/work/bob.vcf" {
		t.Errorf("hrefs = %v, want only the item added since the token", got)
	}
	if ms.SyncToken == "" || ms.SyncToken == token {
		t.Errorf("token did not advance: %q", ms.SyncToken)
	}
}

func TestSyncCollectionReportsDeletionsAsNotFoundRows(t *testing.T) {
	store := newStore(t)
	seedRaw(t, store, "alice", "ada.vcf", adaVCF, "ada")
	h := handlerFor(t, store, carddav.Config{})
	token := reportMS(t, h, "/alice/work/", syncBody("")).SyncToken

	if w := del(h, "/alice/work/ada.vcf", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}

	ms := reportMS(t, h, "/alice/work/", syncBody(token))
	row := ms.at(t, "/alice/work/ada.vcf")
	if row.Status == "" || !strings.Contains(row.Status, "404") {
		t.Errorf("deleted item status = %q, want a 404 row telling the client to drop it", row.Status)
	}
}

func TestSyncCollectionRefusesAForeignToken(t *testing.T) {
	store := newStore(t)
	h := handlerFor(t, store, carddav.Config{})

	// RFC 6578 §3.2: an unserviceable token MUST be DAV:valid-sync-token. A
	// silent full listing carries no deletions, so the client would keep
	// removed items forever.
	for name, token := range map[string]string{
		"garbage":              "not-a-token",
		"another address book": reportMS(t, h, "/alice/work/", syncBody("")).SyncToken + "0",
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
	seedRaw(t, store, "alice", "ada.vcf", adaVCF, "ada")
	h := handlerFor(t, store, carddav.Config{})
	token := reportMS(t, h, "/alice/work/", syncBody("")).SyncToken

	seedRaw(t, store, "alice", "bob.vcf", bobVCF, "bob")
	ref := carddav.AddressBookRef{Account: "alice", Book: carddav.MustSegment("work")}
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
	h := handlerFor(t, readOnlyBackend{store}, carddav.Config{})

	if w := report(t, h, "/alice/work/", syncBody("")); w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

func TestSupportedReportSetMatchesWhatIsDispatched(t *testing.T) {
	store := newStore(t)
	syncing := handlerFor(t, store, carddav.Config{})
	plain := handlerFor(t, readOnlyBackend{store}, carddav.Config{})

	ask := askFor(davName("supported-report-set"))
	full := propfind(t, syncing, "/alice/work/", "0", ask).at(t, "/alice/work/").value(t, davName("supported-report-set"))
	for _, want := range []string{"addressbook-query", "addressbook-multiget", "sync-collection"} {
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
	h := handlerFor(t, newStore(t), carddav.Config{})

	w := report(t, h, "/alice/", filterQuery(`<C:filter><C:prop-filter name="FN"/></C:filter>`))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestMultigetShapesLikeAQuery(t *testing.T) {
	h := handlerFor(t, abStore(t), carddav.Config{})

	// Both reports project through the same code, and only addressbook-query
	// otherwise exercises it.
	body := `<?xml version="1.0"?>
<C:addressbook-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <C:address-data><C:prop name="FN"/></C:address-data>
  </D:prop>
  <D:href>/alice/work/ada.vcf</D:href>
</C:addressbook-multiget>`

	data := reportMS(t, h, "/alice/work/", body).
		at(t, "/alice/work/ada.vcf").value(t, carddavName("address-data"))

	if !strings.Contains(data, "FN:Ada Lovelace") {
		t.Errorf("address-data = %q, want the requested FN", data)
	}
	if strings.Contains(data, "EMAIL") {
		t.Errorf("address-data = %q, multiget must project as a query does", data)
	}
}
