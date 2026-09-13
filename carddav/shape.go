package carddav

import (
	"errors"
	"unicode/utf8"
)

var errNotUTF8 = errors.New("not UTF-8")

// CheckShape reports, as an *InvalidContentError, a body that is unsafe to
// hand to a vCard parser: bytes that are not UTF-8. go-vcard neither nests
// nor, from v0.1.0, parses anything in more than linear time, so that is the
// whole check. It does not bound the body's size; the caller's limit does.
func CheckShape(body []byte) error {
	if !utf8.Valid(body) {
		return &InvalidContentError{Err: errNotUTF8}
	}
	return nil
}
