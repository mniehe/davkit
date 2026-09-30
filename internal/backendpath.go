package internal

import (
	"math"
	"net/http"
	"path"
	"strings"
)

// ValidateMemberPath confirms that memberPath names an immediate member of the
// collection at collectionPath. Paths a Backend returns are emitted to the
// client as DAV:href values, and for query and sync results the object body
// travels with them, so a Backend whose tenant predicate is wrong would
// otherwise disclose another collection's contents.
//
// This is deliberately stricter than ChildHref, which confines client-supplied
// multiget hrefs and admits the collection itself or any descendant. A calendar
// or address book holds its members directly, so a nested path is not one of
// them.
//
// A non-canonical path is rejected rather than cleaned: the Backend broke its
// contract, and silently repairing that hides the defect.
func ValidateMemberPath(collectionPath, memberPath string) error {
	member := strings.TrimSuffix(memberPath, "/")
	if member == "" || !strings.HasPrefix(member, "/") || path.Clean(member) != member {
		return HTTPErrorf(http.StatusInternalServerError, "webdav: backend returned a member path that is not absolute and canonical")
	}
	if path.Dir(member) != strings.TrimSuffix(collectionPath, "/") {
		return HTTPErrorf(http.StatusInternalServerError, "webdav: backend returned a member outside the requested collection")
	}
	return nil
}

// ValidateSyncRequest checks the sync-collection REPORT grammar of RFC 6578
// §3.2, except that Depth: 1 is accepted for client compatibility. The
// DAV:sync-level alone determines the scope, never the Depth header.
//
// Only level 1 is implemented. An infinite request is refused rather than
// answered from a flat result, because returning a sync token alongside a
// narrower set than was asked for tells the client it holds state it was never
// sent, and it has no way to discover the omission later.
func ValidateSyncRequest(r *http.Request, query *SyncCollectionQuery) error {
	if depth := r.Header.Get("Depth"); depth != "" && depth != "0" && depth != "1" {
		return HTTPErrorf(http.StatusBadRequest, "webdav: sync-collection requires Depth 0, 1 or no Depth header")
	}
	switch query.SyncLevel {
	case "1":
		return nil
	case "":
		return HTTPErrorf(http.StatusBadRequest, "webdav: sync-collection requires a DAV:sync-level")
	case "infinite":
		return HTTPErrorf(http.StatusBadRequest, "webdav: sync-collection with an infinite sync-level is not supported")
	default:
		return HTTPErrorf(http.StatusBadRequest, "webdav: unrecognised DAV:sync-level")
	}
}

// ValidateResourcePath confirms that a Backend answered a lookup with the
// resource that was asked for. The returned resource carries its own path, and
// that path becomes the response href, so a Backend answering with a different
// one names a resource the handler never confined — multiget checks the href it
// was given, not the object it gets back.
func ValidateResourcePath(requestedPath, resourcePath string) error {
	if resourcePath != requestedPath {
		return HTTPErrorf(http.StatusInternalServerError, "webdav: backend returned a resource other than the one requested")
	}
	return nil
}

// ValidateCollectionPath is ValidateResourcePath for a collection, where a
// trailing slash names the same resource and Backends differ on whether they
// carry one.
func ValidateCollectionPath(requestedPath, collectionPath string) error {
	return ValidateResourcePath(strings.TrimSuffix(requestedPath, "/"), strings.TrimSuffix(collectionPath, "/"))
}

// RequestedLimit reads a DAV:limit into a count, or nil when the client set
// none. Zero is a request for no members rather than for all of them, so the
// absent case cannot be represented by a sentinel count.
//
// A value too large for an int is refused rather than converted: the conversion
// wraps to a negative number, which every caller would read as "no limit" --
// the opposite of what was asked.
func RequestedLimit(limit *Limit) (*int, error) {
	if limit == nil {
		return nil, nil
	}
	return RequestedCount(limit.NResults)
}

// RequestedCount is RequestedLimit for a limit element outside the DAV:
// namespace, which carries the same nresults value.
func RequestedCount(nresults uint) (*int, error) {
	if nresults > math.MaxInt {
		return nil, HTTPErrorf(http.StatusBadRequest, "webdav: nresults is out of range")
	}
	n := int(nresults) //nolint:gosec // G115: bounded by the check above
	return &n, nil
}

// IsDenied reports whether err refuses access rather than reporting a failure.
//
// The distinction decides what a caller may do with it: a denial conceals one
// member and the rest of the answer stands, while anything else must fail the
// whole request. Treating an outage as a denial would let a Backend hiccup
// quietly shorten a listing that the client then banks as complete.
func IsDenied(err error) bool {
	code := HTTPErrorFromError(err).Code
	return code == http.StatusForbidden || code == http.StatusNotFound
}
