package carddav

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/emersion/go-vcard"
	"github.com/mniehe/davkit/internal"
)

// maxItemBytes is the absolute cap on a PUT body, before the address book's
// own MaxItemSize narrows it. It exists so a hostile client cannot force an
// unbounded read into the parser; real vCards are kilobytes.
const maxItemBytes = 10 << 20

// vcardVersions is what supported-address-data advertises, so it is also what
// a PUT may store: serving back a version never claimed would hand every
// reader data the server said it does not speak.
var vcardVersions = map[string]bool{"3.0": true, "4.0": true}

// parseCardBody validates an address object resource (RFC 6352 §5.1) and
// extracts its identity. The errors are the RFC's own preconditions:
// valid-address-data for anything that is not exactly one well-formed vCard
// with the FN both versions require and the UID this server keys items by,
// and supported-address-data for a version the server never advertised.
func parseCardBody(body []byte) (contentID string, err error) {
	if shapeErr := CheckShape(body); shapeErr != nil {
		return "", internal.NewPreconditionError(http.StatusForbidden, validAddressDataName)
	}
	// The decoder is lenient: it skips bytes before BEGIN:VCARD and after
	// END:VCARD. Stored bytes are served verbatim, so the envelope has to be
	// checked here or a stricter reader than go-vcard gets handed junk.
	if !isSingleCard(body) {
		return "", internal.NewPreconditionError(http.StatusForbidden, validAddressDataName)
	}
	card, err := vcard.NewDecoder(bytes.NewReader(body)).Decode()
	if err != nil {
		return "", internal.NewPreconditionError(http.StatusForbidden, validAddressDataName)
	}

	version := card.Value(vcard.FieldVersion)
	if version == "" {
		return "", internal.NewPreconditionError(http.StatusForbidden, validAddressDataName)
	}
	if !vcardVersions[version] {
		return "", internal.NewPreconditionError(http.StatusForbidden, supportedAddressDataName)
	}
	if card.Value(vcard.FieldFormattedName) == "" {
		return "", internal.NewPreconditionError(http.StatusForbidden, validAddressDataName)
	}
	uid := card.Value(vcard.FieldUID)
	if uid == "" {
		return "", internal.NewPreconditionError(http.StatusForbidden, validAddressDataName)
	}
	return uid, nil
}

// isSingleCard reports whether body is one BEGIN:VCARD…END:VCARD envelope
// with nothing but whitespace around it. Lines starting with space or tab are
// folded continuations of the line before and never delimiters (RFC 6350 §3.2).
func isSingleCard(body []byte) bool {
	sawBegin, sawEnd := false, false
	for _, raw := range bytes.Split(body, []byte("\n")) {
		line := bytes.TrimRight(raw, "\r")
		if len(line) == 0 || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		switch {
		case sawEnd:
			return false
		case !sawBegin:
			if !bytes.EqualFold(line, []byte("BEGIN:VCARD")) {
				return false
			}
			sawBegin = true
		case bytes.EqualFold(line, []byte("END:VCARD")):
			sawEnd = true
		}
	}
	return sawBegin && sawEnd
}

func checkCardContentType(header string) error {
	if header == "" {
		return nil
	}
	t, _, err := mime.ParseMediaType(header)
	if err != nil {
		return internal.HTTPErrorf(http.StatusBadRequest, "carddav: malformed Content-Type: %v", err)
	}
	if t != vcard.MIMEType {
		return internal.NewPreconditionError(http.StatusUnsupportedMediaType, supportedAddressDataName)
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
	if !acc.book.CreateItems && !acc.book.ReplaceItems {
		return denyOperation("write items in this address book")
	}
	if typeErr := checkCardContentType(r.Header.Get("Content-Type")); typeErr != nil {
		return typeErr
	}

	ctx := r.Context()
	book, err := a.getAddressBook(ctx, acc.AddressBookRef())
	if err != nil {
		return backendError(err)
	}

	limit := int64(maxItemBytes)
	if book.MaxItemSize > 0 && book.MaxItemSize < limit {
		limit = book.MaxItemSize
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return internal.NewPreconditionError(http.StatusRequestEntityTooLarge, maxResourceSizeName)
		}
		return fmt.Errorf("carddav: reading request body: %w", err)
	}

	contentID, err := parseCardBody(body)
	if err != nil {
		return err
	}

	scope := bookScope(book.ID)
	pre, err := preconditionsFrom(scope, r.Header.Get("If-Match"), r.Header.Get("If-None-Match"))
	if err != nil {
		return internal.HTTPErrorf(http.StatusBadRequest, "carddav: %v", err)
	}

	writer := a.backend.(ItemWriter) //nolint:errcheck // caps.writesItems was resolved from this assertion at construction
	result, err := writer.CompareAndStoreItem(ctx, acc.ItemRef(), StoreItemRequest{
		Content:       body,
		ContentID:     contentID,
		Preconditions: pre,
		MayCreate:     acc.book.CreateItems,
		MayReplace:    acc.book.ReplaceItems,
	})
	if err != nil {
		var dup *DuplicateContentIDError
		if errors.As(err, &dup) {
			return internal.NewPreconditionError(http.StatusConflict, noUIDConflictName)
		}
		var quota *QuotaExceededError
		if errors.As(err, &quota) {
			return internal.HTTPErrorf(http.StatusInsufficientStorage, "carddav: quota exceeded")
		}
		var invalid *InvalidContentError
		if errors.As(err, &invalid) {
			return internal.NewPreconditionError(http.StatusForbidden, validAddressDataName)
		}
		// The address book was fetched above, so a missing parent is a race with
		// a concurrent deletion — RFC 4918 §9.7.1 answers 409, not 404.
		if errors.Is(err, ErrParentNotFound) {
			return internal.HTTPErrorf(http.StatusConflict, "carddav: the address book was removed while this request ran")
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
		if !acc.book.DeleteItems {
			return denyOperation("delete items in this address book")
		}
	case KindAddressBook:
		if !a.caps.deletesBooks {
			return errUnsupportedMethod(r.Method)
		}
		if !acc.book.DeleteBook {
			return denyOperation("delete this address book")
		}
	default:
		return errUnsupportedMethod(r.Method)
	}

	ctx := r.Context()
	book, err := a.getAddressBook(ctx, acc.AddressBookRef())
	if err != nil {
		return backendError(err)
	}
	pre, err := preconditionsFrom(bookScope(book.ID), r.Header.Get("If-Match"), r.Header.Get("If-None-Match"))
	if err != nil {
		return internal.HTTPErrorf(http.StatusBadRequest, "carddav: %v", err)
	}

	if acc.Kind == KindAddressBook {
		deleter := a.backend.(BookDeleter) //nolint:errcheck // caps.deletesBooks was resolved from this assertion at construction
		if err := deleter.CompareAndDeleteAddressBook(ctx, acc.AddressBookRef(), pre); err != nil {
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
