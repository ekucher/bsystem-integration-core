package adapters

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// Page requests a bounded window of an upstream collection.
//
// The cursor is opaque by design. It encodes an offset today because every
// upstream BSYSTEM integrates with paginates by offset, but callers must not
// read or construct one: when an upstream gains real cursors, only this file
// changes.
type Page struct {
	// Limit is the maximum number of items to return. Zero selects the
	// adapter's default.
	Limit int
	// Cursor is an opaque position from a previous PageInfo. Empty starts at
	// the beginning.
	Cursor string
}

// PageInfo describes the window an adapter returned.
type PageInfo struct {
	// Total is the size of the whole collection, when the upstream reports
	// one.
	Total int
	// NextCursor is the position of the following page, or empty when the
	// collection is exhausted.
	NextCursor string
}

// ErrInvalidCursor means the caller supplied a cursor this platform did not
// issue.
var ErrInvalidCursor = errors.New("invalid pagination cursor")

const cursorPrefix = "o:"

// EncodeCursor returns the opaque cursor for an offset.
func EncodeCursor(offset int) string {
	if offset <= 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + strconv.Itoa(offset)))
}

// DecodeCursor returns the offset a cursor names.
func DecodeCursor(cursor string) (int, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, ErrInvalidCursor
	}
	body, found := strings.CutPrefix(string(decoded), cursorPrefix)
	if !found {
		return 0, ErrInvalidCursor
	}
	offset, err := strconv.Atoi(body)
	if err != nil || offset < 0 {
		return 0, ErrInvalidCursor
	}
	return offset, nil
}

// ClampLimit bounds a requested page size.
//
// An out-of-range limit is clamped rather than rejected: a caller asking for
// more than the platform will serve gets the maximum, not an error. The cap
// is what stops one request pulling an unbounded amount of upstream data.
func ClampLimit(limit, fallback, max int) int {
	if limit <= 0 {
		limit = fallback
	}
	if limit > max {
		limit = max
	}
	return limit
}

// Resolve turns a page request into the offset and limit an upstream call
// needs, applying the adapter's own bounds.
func (p Page) Resolve(defaultLimit, maxLimit int) (offset, limit int, err error) {
	offset, err = DecodeCursor(p.Cursor)
	if err != nil {
		return 0, 0, err
	}
	return offset, ClampLimit(p.Limit, defaultLimit, maxLimit), nil
}

// NewPageInfo builds the description of a returned window.
//
// A next cursor is issued only when the upstream's own total says more
// remains. A short page also ends the collection: asking again would return
// nothing and the caller would loop.
func NewPageInfo(total, offset, returned int) PageInfo {
	info := PageInfo{Total: total}
	if returned > 0 && offset+returned < total {
		info.NextCursor = EncodeCursor(offset + returned)
	}
	return info
}
