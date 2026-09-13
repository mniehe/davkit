package caldav

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"
)

// Bounds on a calendar object's shape. go-ical has none of its own: it
// recurses once per nested component and builds each parameter value a byte
// at a time. Real data nests three deep (VCALENDAR, VEVENT, VALARM) and its
// longest parameter values, such as Apple's map handles, run to a few KB.
const (
	maxComponentDepth  = 8
	maxComponents      = 1000
	maxParamValueBytes = 8 << 10
)

var (
	errNotUTF8           = errors.New("not UTF-8")
	errTooDeep           = fmt.Errorf("components nest deeper than %d", maxComponentDepth)
	errTooManyComponents = fmt.Errorf("more than %d components", maxComponents)
	errLongParamValue    = fmt.Errorf("a parameter value longer than %d bytes", maxParamValueBytes)
	errUnendedParam      = errors.New("a parameter value not followed by ',', ';' or ':'")

	// lineFold is where RFC 5545 §3.1 folds a line, which it may do anywhere,
	// even inside a property name.
	lineFold = regexp.MustCompile(`\r?\n[ \t]`)

	beginName = []byte("BEGIN")
	endName   = []byte("END")
)

// CheckShape reports, as an *InvalidContentError, a body that is unsafe to
// hand to an iCalendar parser: bytes that are not UTF-8, components nested or
// repeated past the bounds above, an over-long parameter value, or a
// parameter go-ical would panic on. It reads each line the way go-ical does:
// unfolded, with the name upper-cased and cut at the first ';' or ':'. It
// does not bound the body's size; the caller's limit does.
func CheckShape(body []byte) error {
	if !utf8.Valid(body) {
		return &InvalidContentError{Err: errNotUTF8}
	}

	var depth, components int
	for line := range bytes.Lines(lineFold.ReplaceAll(body, nil)) {
		name, params := splitName(bytes.TrimRight(line, "\r\n"))
		switch {
		case bytes.Equal(name, beginName):
			depth++
			components++
		case bytes.Equal(name, endName):
			depth--
		}

		if depth > maxComponentDepth {
			return &InvalidContentError{Err: errTooDeep}
		}
		if components > maxComponents {
			return &InvalidContentError{Err: errTooManyComponents}
		}
		if err := checkParams(params); err != nil {
			return &InvalidContentError{Err: err}
		}
	}
	return nil
}

// splitName returns a content line's upper-cased name and what follows it. A
// line with neither ';' nor ':' has no name go-ical would accept.
func splitName(line []byte) (name, rest []byte) {
	end := bytes.IndexAny(line, ";:")
	if end < 0 {
		return nil, nil
	}
	return bytes.ToUpper(line[:end]), line[end:]
}

// checkParams walks the parameters at the start of rest as go-ical's
// decodeParam does. A shape go-ical refuses with an error of its own is left
// to it.
func checkParams(rest []byte) error {
	for len(rest) > 0 && rest[0] == ';' {
		eq := bytes.IndexByte(rest, '=')
		if eq < 0 {
			return nil
		}

		var err error
		if rest, err = checkParamValues(rest[eq+1:]); err != nil || rest == nil {
			return err
		}
	}
	return nil
}

// checkParamValues walks one parameter's comma-separated values and returns
// what follows them, or nil where go-ical would stop with its own error.
func checkParamValues(s []byte) ([]byte, error) {
	for {
		value, rest, ok := cutParamValue(s)
		if !ok {
			return nil, nil
		}
		if len(value) > maxParamValueBytes {
			return nil, errLongParamValue
		}
		if len(rest) == 0 {
			return nil, errUnendedParam
		}

		switch rest[0] {
		case ',':
			s = rest[1:]
		case ';', ':':
			return rest, nil
		default:
			return nil, errUnendedParam
		}
	}
}

// cutParamValue splits one parameter value from what follows it; ok is false
// where go-ical would return an error rather than a value.
func cutParamValue(s []byte) (value, rest []byte, ok bool) {
	if len(s) > 0 && s[0] == '"' {
		end := bytes.IndexByte(s[1:], '"')
		if end < 0 {
			return nil, nil, false
		}
		return s[1 : 1+end], s[2+end:], true
	}

	end := bytes.IndexAny(s, "\";,:")
	if end < 0 {
		return s, nil, true
	}
	if s[end] == '"' {
		return nil, nil, false
	}
	return s[:end], s[end:], true
}
