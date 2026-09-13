package caldav

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/emersion/go-ical"
	"github.com/mniehe/davkit/internal"
)

// maxItemBytes is the absolute cap on a PUT body, before the calendar's own
// MaxItemSize narrows it. It exists so a hostile client cannot force an
// unbounded read into the parser; real calendar objects are kilobytes.
const maxItemBytes = 10 << 20

// parsedItem is what body validation extracts: the facts a backend stores
// without ever holding a parser.
type parsedItem struct {
	contentID string
	kind      ItemKind
}

var kindByComponent = map[string]ItemKind{
	ical.CompEvent:    Event,
	ical.CompToDo:     Task,
	ical.CompJournal:  Note,
	ical.CompFreeBusy: Availability,
}

// parseItemBody validates a calendar object resource (RFC 4791 §4.1) and
// extracts its identity. The errors are the RFC's own preconditions:
// valid-calendar-data for bytes that are not iCalendar at all, and
// valid-calendar-object-resource for iCalendar that is not storable — a
// scheduling METHOD, mixed component kinds, or components that do not agree on
// one UID.
func parseItemBody(body []byte) (parsedItem, error) {
	if err := CheckShape(body); err != nil {
		return parsedItem{}, internal.NewPreconditionError(http.StatusForbidden, validCalendarDataName)
	}

	dec := ical.NewDecoder(bytes.NewReader(body))
	cal, err := dec.Decode()
	if err != nil {
		return parsedItem{}, internal.NewPreconditionError(http.StatusForbidden, validCalendarDataName)
	}
	// A calendar object resource is exactly one VCALENDAR (RFC 4791 §4.1). The
	// decoder stops at the first END:VCALENDAR, so a second object hides behind
	// it — stored verbatim and served by GET, but invisible to reports, UID
	// uniqueness and filtering, which only ever decode the first. Require the
	// stream to end after one, as carddav does for vCards.
	if _, next := dec.Decode(); !errors.Is(next, io.EOF) {
		return parsedItem{}, internal.NewPreconditionError(http.StatusForbidden, validCalendarDataName)
	}
	// RFC 5545 §3.6 requires VERSION and PRODID in every object, and DTSTAMP
	// in every component below. The encoder refuses to write anything missing
	// them, so accepting one here would store an object no report could serve.
	if cal.Props.Get(ical.PropVersion) == nil || cal.Props.Get(ical.PropProductID) == nil {
		return parsedItem{}, internal.NewPreconditionError(http.StatusForbidden, validCalendarDataName)
	}
	if cal.Props.Get(ical.PropMethod) != nil {
		return parsedItem{}, internal.NewPreconditionError(http.StatusForbidden, validCalendarObjectName)
	}

	var item parsedItem
	for _, comp := range cal.Children {
		if comp.Name == ical.CompTimezone {
			continue
		}
		kind, known := kindByComponent[comp.Name]
		if !known {
			return parsedItem{}, internal.NewPreconditionError(http.StatusForbidden, validCalendarObjectName)
		}
		uidProp := comp.Props.Get(ical.PropUID)
		if uidProp == nil || uidProp.Value == "" {
			return parsedItem{}, internal.NewPreconditionError(http.StatusForbidden, validCalendarObjectName)
		}
		if comp.Props.Get(ical.PropDateTimeStamp) == nil {
			return parsedItem{}, internal.NewPreconditionError(http.StatusForbidden, validCalendarDataName)
		}
		if item.kind == 0 {
			item = parsedItem{contentID: uidProp.Value, kind: kind}
			continue
		}
		// A master and its overridden instances share a kind and a UID; that is
		// one item. Anything else is two items in one resource.
		if kind != item.kind || uidProp.Value != item.contentID {
			return parsedItem{}, internal.NewPreconditionError(http.StatusForbidden, validCalendarObjectName)
		}
	}
	if item.kind == 0 {
		return parsedItem{}, internal.NewPreconditionError(http.StatusForbidden, validCalendarObjectName)
	}
	return item, nil
}

func checkItemContentType(header string) error {
	if header == "" {
		return nil
	}
	t, _, err := mime.ParseMediaType(header)
	if err != nil {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: malformed Content-Type: %v", err)
	}
	if t != ical.MIMEType {
		return internal.NewPreconditionError(http.StatusUnsupportedMediaType, supportedCalendarDataName)
	}
	return nil
}

func (a *adapter) Put(w http.ResponseWriter, r *http.Request) error {
	acc, err := a.resolve(r)
	if err != nil {
		return err
	}
	if acc.Kind != KindItem || !a.caps.writesItems {
		return errUnsupportedMethod(r.Method)
	}
	if !acc.calendar.CreateItems && !acc.calendar.ReplaceItems {
		return denyOperation("write items in this calendar")
	}
	if typeErr := checkItemContentType(r.Header.Get("Content-Type")); typeErr != nil {
		return typeErr
	}

	ctx := r.Context()
	cal, err := a.getCalendar(ctx, acc.CalendarRef())
	if err != nil {
		return backendError(err)
	}

	limit := int64(maxItemBytes)
	if cal.MaxItemSize > 0 && cal.MaxItemSize < limit {
		limit = cal.MaxItemSize
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return internal.NewPreconditionError(http.StatusRequestEntityTooLarge, maxResourceSizeName)
		}
		return fmt.Errorf("caldav: reading request body: %w", err)
	}

	item, err := parseItemBody(body)
	if err != nil {
		return err
	}
	if !cal.Accepts.Allows(item.kind) {
		return internal.NewPreconditionError(http.StatusForbidden, supportedCalendarComponentName)
	}

	scope := calendarScope(cal.ID)
	pre, err := preconditionsFrom(scope, r.Header.Get("If-Match"), r.Header.Get("If-None-Match"))
	if err != nil {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: %v", err)
	}

	writer := a.backend.(ItemWriter) //nolint:errcheck // caps.writesItems was resolved from this assertion at construction
	result, err := writer.CompareAndStoreItem(ctx, acc.ItemRef(), StoreItemRequest{
		Content:       body,
		ContentID:     item.contentID,
		Kind:          item.kind,
		Preconditions: pre,
		MayCreate:     acc.calendar.CreateItems,
		MayReplace:    acc.calendar.ReplaceItems,
	})
	if err != nil {
		var dup *DuplicateContentIDError
		if errors.As(err, &dup) {
			return internal.NewPreconditionError(http.StatusConflict, noUIDConflictName)
		}
		var quota *QuotaExceededError
		if errors.As(err, &quota) {
			return internal.HTTPErrorf(http.StatusInsufficientStorage, "caldav: quota exceeded")
		}
		var invalid *InvalidContentError
		if errors.As(err, &invalid) {
			return internal.NewPreconditionError(http.StatusForbidden, validCalendarDataName)
		}
		// The calendar was fetched above, so a missing parent is a race with a
		// concurrent deletion — RFC 4918 §9.7.1 answers 409, not 404.
		if errors.Is(err, ErrParentNotFound) {
			return internal.HTTPErrorf(http.StatusConflict, "caldav: the calendar was removed while this request ran")
		}
		return backendError(err)
	}
	if result.Revision == 0 {
		// The ETag below would fall back to a content hash that no later
		// If-Match could parse, stranding the client on its next write.
		return contractViolation("a writing backend stored item %q with a zero revision", acc.ItemRef().Item)
	}

	w.Header().Set("ETag", etagFor(scope, Item{Content: body, Revision: result.Revision}).String())
	if result.Created {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
	return nil
}

func (a *adapter) Delete(r *http.Request) error {
	acc, err := a.resolve(r)
	if err != nil {
		return err
	}
	switch acc.Kind {
	case KindItem:
		if !a.caps.writesItems {
			return errUnsupportedMethod(r.Method)
		}
		if !acc.calendar.DeleteItems {
			return denyOperation("delete items in this calendar")
		}
	case KindCalendar:
		if !a.caps.deletesCalendars {
			return errUnsupportedMethod(r.Method)
		}
		if !acc.calendar.DeleteCalendar {
			return denyOperation("delete this calendar")
		}
	default:
		return errUnsupportedMethod(r.Method)
	}

	ctx := r.Context()
	cal, err := a.getCalendar(ctx, acc.CalendarRef())
	if err != nil {
		return backendError(err)
	}
	pre, err := preconditionsFrom(calendarScope(cal.ID), r.Header.Get("If-Match"), r.Header.Get("If-None-Match"))
	if err != nil {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: %v", err)
	}

	if acc.Kind == KindCalendar {
		deleter := a.backend.(CalendarDeleter) //nolint:errcheck // caps.deletesCalendars was resolved from this assertion at construction
		if err := deleter.CompareAndDeleteCalendar(ctx, acc.CalendarRef(), pre); err != nil {
			return backendError(err)
		}
		return nil
	}
	writer := a.backend.(ItemWriter) //nolint:errcheck // caps.writesItems was resolved from this assertion at construction
	if err := writer.CompareAndDeleteItem(ctx, acc.ItemRef(), pre); err != nil {
		return backendError(err)
	}
	return nil
}
