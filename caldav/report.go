package caldav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/emersion/go-ical"
	"github.com/mniehe/davkit/internal"
)

// handleReport dispatches a REPORT. internal.Handler does not know the method,
// so Handler.ServeHTTP routes it here before delegating.
//
// Every report is served against a calendar collection. RFC 4791 also permits
// query and multiget against a single item, which no mainstream client sends;
// an item target answers 405 and Allow says the same.
func (a *adapter) handleReport(w http.ResponseWriter, r *http.Request) error {
	acc, err := a.resolve(r)
	if err != nil {
		return err
	}
	if acc.Kind != KindCalendar {
		return errUnsupportedMethod(r.Method)
	}

	var report reportReq
	if err := internal.DecodeXMLRequestWithin(r, &report, internal.RequestNodeBudget(0)); err != nil {
		return err
	}

	// free-busy-query reveals only busy times, so it is the one report a
	// ViewAvailability-only sharee may run; everything else reads the items.
	if report.FreeBusy == nil && !acc.calendar.ViewDetails {
		return denyOperation("read the items in this calendar")
	}

	switch {
	case report.FreeBusy != nil:
		if !acc.calendar.ViewAvailability {
			return denyOperation("learn this calendar's busy times")
		}
		return a.reportFreeBusy(w, r, acc, report.FreeBusy)
	case report.Query != nil:
		if err := internal.BoundReportProp(report.Query.Prop, report.Query.AllProp, report.Query.PropName); err != nil {
			return err
		}
		return a.reportQuery(w, r, acc, report.Query)
	case report.Multiget != nil:
		if err := internal.BoundReportProp(report.Multiget.Prop, report.Multiget.AllProp, report.Multiget.PropName); err != nil {
			return err
		}
		if err := internal.BoundHrefs(report.Multiget.Hrefs, 0); err != nil {
			return err
		}
		if err := internal.BoundResponseWork(internal.SelectorEchoSize(report.Multiget.Prop, report.Multiget.AllProp, report.Multiget.PropName), len(report.Multiget.Hrefs), 0); err != nil {
			return err
		}
		return a.reportMultiget(w, r, acc, report.Multiget)
	case report.SyncCollection != nil:
		// sync-collection tolerates a missing selector (a fallback below), so
		// only the echo needs bounding here.
		if err := internal.BoundPropNames(report.SyncCollection.Prop); err != nil {
			return err
		}
		return a.reportSync(w, r, acc, report.SyncCollection)
	default:
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: expected calendar-query, calendar-multiget or sync-collection element in REPORT request")
	}
}

// reportObject is one item as a report serves it: the stored fact plus the
// parsed copy the engine works on.
type reportObject struct {
	href string
	etag internal.ETag
	data *ical.Calendar
}

// parseStored parses an item's bytes for matching. The library validated
// everything it stored, but a read-only backend can hold bytes it never saw —
// and those cannot be matched, so the answer is a loud contract violation
// rather than a listing that silently omits a member.
func (a *adapter) parseStored(ctx context.Context, ref ItemRef, item Item, scope uint64) (reportObject, error) {
	data, err := ical.NewDecoder(bytes.NewReader(item.Content)).Decode()
	if err != nil {
		return reportObject{}, fmt.Errorf("caldav: item %q holds bytes that do not parse as iCalendar: %w", ref.Item, err)
	}
	href, err := a.cfg.Routes.Href(ctx, ItemResource(ref))
	if err != nil {
		return reportObject{}, fmt.Errorf("caldav: rendering item href: %w", err)
	}
	return reportObject{href: href, etag: etagFor(scope, item), data: data}, nil
}

// reportResponse renders one report row. Unlike itemResponse it serves the
// engine's output — projected or expanded — rather than the stored bytes.
func reportResponse(obj reportObject, pf *internal.PropFind) (*internal.Response, error) {
	var buf bytes.Buffer
	if err := encodeCalendarData(&buf, obj.data); err != nil {
		return nil, fmt.Errorf("caldav: encoding report calendar-data: %w", err)
	}

	props := map[xml.Name]internal.PropFindFunc{
		internal.ResourceTypeName:     internal.PropFindValue(internal.NewResourceType()),
		internal.GetETagName:          internal.PropFindValue(&internal.GetETag{ETag: obj.etag}),
		internal.GetContentTypeName:   internal.PropFindValue(&internal.GetContentType{Type: calendarMediaType}),
		internal.GetContentLengthName: internal.PropFindValue(&internal.GetContentLength{Length: int64(buf.Len())}),
		calendarDataName:              internal.PropFindValue(&calendarData{Data: buf.Bytes()}),
	}
	internal.RemoveFromAllProp(pf, props, calendarDataName)
	return internal.NewPropFindResponse(obj.href, pf, props)
}

// dataRequestFrom pulls the calendar-data projection out of a report's prop
// selector. Absent means the whole object.
func dataRequestFrom(prop *internal.Prop) (calendarCompRequest, error) {
	if prop == nil {
		return calendarCompRequest{AllProps: true, AllComps: true}, nil
	}
	var raw calendarDataReq
	err := prop.Decode(&raw)
	if internal.IsNotFound(err) {
		return calendarCompRequest{AllProps: true, AllComps: true}, nil
	}
	if err != nil {
		// The element being re-decoded is client XML, so a failure here is the
		// request's fault, not the server's.
		return calendarCompRequest{}, internal.HTTPErrorf(http.StatusBadRequest, "caldav: %v", err)
	}
	decoded, err := decodeCalendarDataReq(&raw)
	if err != nil {
		return calendarCompRequest{}, err
	}
	return *decoded, nil
}

// shape applies the client's expansion and projection to one parsed object,
// leaving the original untouched.
func shape(obj reportObject, req *calendarCompRequest, budget *expansionBudget) (reportObject, error) {
	shaped := obj.data
	if req.Expand != nil {
		expanded, err := expandCalendarWithin(shaped, req.Expand, budget, forReport)
		if err != nil {
			return reportObject{}, err
		}
		shaped = expanded
	}
	if req.LimitRecurrence != nil {
		limited, err := limitRecurrenceCalendar(shaped, req.LimitRecurrence)
		if err != nil {
			return reportObject{}, err
		}
		shaped = limited
	}
	if req.LimitFreeBusy != nil {
		limited, err := limitFreeBusyCalendar(shaped, req.LimitFreeBusy)
		if err != nil {
			return reportObject{}, err
		}
		shaped = limited
	}
	obj.data = projectCalendar(shaped, req)
	return obj, nil
}

// queryTimezone chooses the zone a query resolves floating times against: the
// request's CALDAV:timezone if it carries one (RFC 4791 §7.8), otherwise the
// calendar's stored default, otherwise none — in which case floating times are
// read as UTC. A malformed request timezone is the client's error; a stored
// default that no longer parses is not, so it is passed over rather than raised.
func queryTimezone(spec string, calendarDefault Timezone) (*tzResolver, error) {
	if strings.TrimSpace(spec) != "" {
		r, err := resolverFromICS([]byte(spec))
		if err != nil {
			return nil, internal.HTTPErrorf(http.StatusBadRequest, "caldav: %v", err)
		}
		return r, nil
	}
	if !calendarDefault.IsZero() {
		if r, err := resolverFromICS(calendarDefault.Bytes()); err == nil {
			return r, nil
		}
	}
	return nil, nil
}

func (a *adapter) reportQuery(w http.ResponseWriter, r *http.Request, acc access, query *calendarQueryReq) error {
	ctx := r.Context()

	dataReq, err := dataRequestFrom(query.Prop)
	if err != nil {
		return err
	}
	compFilter, err := decodeCompFilter(&query.Filter.CompFilter)
	if err != nil {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: %v", err)
	}

	cal, err := a.getCalendar(ctx, acc.CalendarRef())
	if err != nil {
		return backendError(err)
	}
	scope := calendarScope(cal.ID)

	floatingTZ, err := queryTimezone(query.Timezone, cal.Timezone)
	if err != nil {
		return err
	}

	items, _, err := a.listItems(ctx, acc.CalendarRef())
	if err != nil {
		return backendError(err)
	}

	var matched []reportObject
	for i := range items {
		obj, err := a.parseStored(ctx, ItemRef{Calendar: acc.CalendarRef(), Item: items[i].Name}, items[i], scope)
		if err != nil {
			return err
		}
		// Match against a copy whose embedded-VTIMEZONE times are resolved to
		// UTC and whose floating times are read against the query timezone; the
		// stored obj.data is left untouched so a projection returns the object as
		// it was written.
		matchData, err := resolveObjectTimes(obj.data, floatingTZ)
		if err != nil {
			return err
		}
		ok, err := match(compFilter, matchData.Component)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		matched = append(matched, obj)
		if len(matched) > a.cfg.MaxSearchResults {
			return internal.NewPreconditionError(http.StatusInsufficientStorage, internal.NumberOfMatchesWithinLimitsName)
		}
	}
	if err := internal.BoundResponseWork(internal.SelectorEchoSize(query.Prop, query.AllProp, query.PropName), len(matched), 0); err != nil {
		return err
	}

	pf := internal.PropFind{Prop: query.Prop, AllProp: query.AllProp, PropName: query.PropName}
	ms := internal.NewMultiStatusWriter(w, r.URL.Path, 0)
	defer ms.Abort() //nolint:errcheck // best-effort close if the document was already started

	budget := newExpansionBudget()
	for _, obj := range matched {
		shaped, err := shape(obj, &dataReq, budget)
		if err != nil {
			if !ms.Started() {
				return err
			}
			return ms.Fail(err)
		}
		resp, err := reportResponse(shaped, &pf)
		if err != nil {
			if !ms.Started() {
				return err
			}
			return ms.Fail(err)
		}
		if err := ms.Write(resp); err != nil {
			return err
		}
	}
	return ms.Close()
}

func (a *adapter) reportMultiget(w http.ResponseWriter, r *http.Request, acc access, multiget *calendarMultiget) error {
	ctx := r.Context()

	dataReq, err := dataRequestFrom(multiget.Prop)
	if err != nil {
		return err
	}
	cal, err := a.getCalendar(ctx, acc.CalendarRef())
	if err != nil {
		return backendError(err)
	}
	scope := calendarScope(cal.ID)

	pf := internal.PropFind{Prop: multiget.Prop, AllProp: multiget.AllProp, PropName: multiget.PropName}
	ms := internal.NewMultiStatusWriter(w, r.URL.Path, 0)
	defer ms.Abort() //nolint:errcheck // best-effort close if the document was already started

	budget := newExpansionBudget()
	for i := range multiget.Hrefs {
		href := &multiget.Hrefs[i]

		// RFC 4791 §7.9: hrefs must name members of the request collection.
		// Anything outside it is refused per-row and never fetched — the client
		// named the resource, so the refusal tells it nothing it did not supply.
		res, ok := a.resolveMember(ctx, r, acc, href)
		if !ok {
			resp := internal.NewErrorResponse(href.Path, internal.HTTPErrorf(http.StatusForbidden, "caldav: href %q is outside the request collection", href.Path))
			if err := ms.Write(resp); err != nil {
				return err
			}
			continue
		}

		item, err := a.getItem(ctx, res.ItemRef())
		if err == nil {
			var obj reportObject
			if obj, err = a.parseStored(ctx, res.ItemRef(), item, scope); err == nil {
				var shaped reportObject
				if shaped, err = shape(obj, &dataReq, budget); err == nil {
					var resp *internal.Response
					if resp, err = reportResponse(shaped, &pf); err == nil {
						if writeErr := ms.Write(resp); writeErr != nil {
							return writeErr
						}
						continue
					}
				}
			}
		}
		// A contract violation is the handler's problem, not the row's: the
		// rows around a 500 row would read as a complete answer.
		if errors.Is(err, errContract) {
			if !ms.Started() {
				return err
			}
			return ms.Fail(err)
		}
		if writeErr := ms.Write(internal.NewErrorResponse(res.href, backendError(err))); writeErr != nil {
			return writeErr
		}
	}
	return ms.Close()
}

// memberResource is a multiget href resolved to a member of the request
// collection, keeping the path the row is reported under.
type memberResource struct {
	Resource
	href string
}

func (a *adapter) resolveMember(ctx context.Context, r *http.Request, acc access, href *internal.Href) (memberResource, bool) {
	cleaned, ok := internal.ChildHref(r, r.URL.Path, href)
	if !ok {
		return memberResource{}, false
	}
	res, err := a.cfg.Routes.Parse(ctx, (*url.URL)(href).EscapedPath())
	if err != nil || res.Kind != KindItem || res.CalendarRef() != acc.CalendarRef() {
		return memberResource{}, false
	}
	return memberResource{Resource: res, href: cleaned}, true
}

func (a *adapter) reportSync(w http.ResponseWriter, r *http.Request, acc access, query *internal.SyncCollectionQuery) error {
	if !a.caps.syncs {
		return internal.HTTPErrorf(http.StatusNotImplemented, "caldav: sync-collection is not supported by this backend")
	}
	if err := internal.ValidateSyncRequest(r, query); err != nil {
		return err
	}
	limit, err := internal.RequestedLimit(query.Limit)
	if err != nil {
		return err
	}
	dataReq, err := dataRequestFrom(query.Prop)
	if err != nil {
		return err
	}

	ctx := r.Context()
	cal, err := a.getCalendar(ctx, acc.CalendarRef())
	if err != nil {
		return backendError(err)
	}
	scope := calendarScope(cal.ID)

	updated, deleted, coveredThrough, truncated, err := a.syncDelta(ctx, acc.CalendarRef(), scope, query.SyncToken, limit)
	if err != nil {
		return err
	}

	// RFC 6578 §3.7: a server that cannot hold the result at or below the
	// requested limit must fail rather than answer with more than was asked
	// for. Trimming here instead would leave the token describing members that
	// were dropped.
	if limit != nil && len(updated)+len(deleted) > *limit {
		return internal.NewPreconditionError(http.StatusInsufficientStorage, internal.NumberOfMatchesWithinLimitsName)
	}

	// A sync-collection request always carries a <prop>; fall back to allprop
	// so a malformed body without one still yields a usable response.
	pf := internal.PropFind{Prop: query.Prop}
	if query.Prop == nil {
		pf.AllProp = &struct{}{}
	}
	if err := internal.BoundResponseWork(internal.SelectorEchoSize(pf.Prop, pf.AllProp, pf.PropName), len(updated)+len(deleted), 0); err != nil {
		return err
	}

	ms := internal.NewMultiStatusWriter(w, r.URL.Path, 0)
	defer ms.Abort() //nolint:errcheck // best-effort close if the document was already started

	budget := newExpansionBudget()
	for i := range updated {
		obj, err := a.parseStored(ctx, updated[i].ref, updated[i].item, scope)
		if err == nil {
			obj, err = shape(obj, &dataReq, budget)
		}
		var resp *internal.Response
		if err == nil {
			resp, err = reportResponse(obj, &pf)
		}
		if err != nil {
			if !ms.Started() {
				return err
			}
			return ms.Fail(err)
		}
		if err := ms.Write(resp); err != nil {
			return err
		}
	}
	for _, gone := range deleted {
		// RFC 6578 §3.2: a removed member is a 404 row with no properties,
		// telling the client to drop it.
		resp := internal.Response{
			Hrefs:  []internal.Href{{Path: gone}},
			Status: &internal.Status{Code: http.StatusNotFound},
		}
		if err := ms.Write(&resp); err != nil {
			return err
		}
	}
	if truncated {
		if err := ms.Truncate(); err != nil {
			return err
		}
	}
	ms.SetSyncToken(syncTokenFor(scope, coveredThrough))
	return ms.Close()
}

type syncMember struct {
	ref  ItemRef
	item Item
}

// syncDelta answers "what changed since this token". An empty token is the
// initial sync: a full consistent listing, whose revision the new token
// describes. Anything unserviceable — foreign, garbled, older than retained
// history — MUST become DAV:valid-sync-token rather than a silent full
// listing, which carries no deletions and would leave removed items on the
// client forever.
func (a *adapter) syncDelta(ctx context.Context, ref CalendarRef, scope uint64, token string, limit *int) (updated []syncMember, deleted []string, coveredThrough Revision, truncated bool, err error) {
	if token == "" {
		items, rev, listErr := a.listItems(ctx, ref)
		if listErr != nil {
			return nil, nil, 0, false, backendError(listErr)
		}
		for i := range items {
			updated = append(updated, syncMember{ref: ItemRef{Calendar: ref, Item: items[i].Name}, item: items[i]})
		}
		return updated, nil, rev, false, nil
	}

	after, ok := parseSyncToken(scope, token)
	if !ok {
		return nil, nil, 0, false, internal.NewInvalidSyncTokenError()
	}

	maxChanges := 0
	if limit != nil {
		maxChanges = *limit
	}
	sync := a.backend.(SyncBackend) //nolint:errcheck // caps.syncs was resolved from this assertion at construction
	batch, err := sync.ListChanges(ctx, ref, after, maxChanges)
	if err != nil {
		if errors.Is(err, ErrHistoryTooOld) {
			return nil, nil, 0, false, internal.NewInvalidSyncTokenError()
		}
		return nil, nil, 0, false, backendError(err)
	}
	if vErr := validateChangeBatch(after, batch); vErr != nil {
		return nil, nil, 0, false, vErr
	}

	for _, change := range batch.Changes {
		itemRef := ItemRef{Calendar: ref, Item: change.Item}
		if change.Deleted {
			href, hrefErr := a.cfg.Routes.Href(ctx, ItemResource(itemRef))
			if hrefErr != nil {
				return nil, nil, 0, false, fmt.Errorf("caldav: rendering deleted member href: %w", hrefErr)
			}
			deleted = append(deleted, href)
			continue
		}
		item, getErr := a.getItem(ctx, itemRef)
		if getErr != nil {
			return nil, nil, 0, false, backendError(getErr)
		}
		updated = append(updated, syncMember{ref: itemRef, item: item})
	}
	return updated, deleted, batch.CoveredThrough, batch.HasMore, nil
}
