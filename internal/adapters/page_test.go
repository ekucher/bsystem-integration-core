package adapters

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// malformedCursor builds a well-formed base64 cursor around a bad body, which
// is the interesting case for DecodeCursor: a forgery that decodes but does
// not mean anything.
func malformedCursor(body string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + body))
}

// A cursor is opaque by contract. Callers must not be able to read a position
// out of one or construct one, or they will depend on today's encoding and
// break when an upstream gains real cursors.
func TestCursorsAreOpaque(t *testing.T) {
	cursor := EncodeCursor(1234)
	if cursor == "" {
		t.Fatal("a non-zero offset must produce a cursor")
	}
	if strings.Contains(cursor, "1234") {
		t.Fatalf("cursor %q exposes its offset in plain text", cursor)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	for _, offset := range []int{1, 2, 50, 1000, 999999} {
		decoded, err := DecodeCursor(EncodeCursor(offset))
		if err != nil {
			t.Fatalf("decode cursor for offset %d: %v", offset, err)
		}
		if decoded != offset {
			t.Fatalf("round trip gave %d, want %d", decoded, offset)
		}
	}
}

// The first page has no cursor, so a zero offset must not produce one: a
// caller would otherwise be handed a cursor that means "start again".
func TestZeroOffsetHasNoCursor(t *testing.T) {
	if cursor := EncodeCursor(0); cursor != "" {
		t.Fatalf("EncodeCursor(0) = %q, want empty", cursor)
	}
	if cursor := EncodeCursor(-5); cursor != "" {
		t.Fatalf("EncodeCursor(-5) = %q, want empty", cursor)
	}
}

func TestDecodeCursorRejectsForgeries(t *testing.T) {
	tests := []struct {
		name    string
		cursor  string
		wantErr bool
	}{
		{name: "empty means the first page", cursor: ""},
		{name: "whitespace means the first page", cursor: "  "},
		{name: "issued by the platform", cursor: EncodeCursor(10)},
		{name: "not base64", cursor: "not a cursor!", wantErr: true},
		{name: "base64 of something else", cursor: "aGVsbG8", wantErr: true},
		{name: "a raw offset", cursor: "100", wantErr: true},
		{name: "a negative offset", cursor: malformedCursor("-5"), wantErr: true},
		{name: "a non-numeric offset", cursor: malformedCursor("abc"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeCursor(test.cursor)
			if test.wantErr != (err != nil) {
				t.Fatalf("DecodeCursor(%q) error = %v, wantErr = %v", test.cursor, err, test.wantErr)
			}
			if test.wantErr && !errors.Is(err, ErrInvalidCursor) {
				t.Fatalf("error = %v, want ErrInvalidCursor", err)
			}
		})
	}
}

func TestClampLimit(t *testing.T) {
	tests := []struct {
		name                 string
		limit, fallback, max int
		want                 int
	}{
		{name: "unset uses the fallback", limit: 0, fallback: 50, max: 200, want: 50},
		{name: "negative uses the fallback", limit: -1, fallback: 50, max: 200, want: 50},
		{name: "in range is honoured", limit: 25, fallback: 50, max: 200, want: 25},
		{name: "at the cap is honoured", limit: 200, fallback: 50, max: 200, want: 200},
		{name: "above the cap is clamped", limit: 1_000_000, fallback: 50, max: 200, want: 200},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClampLimit(test.limit, test.fallback, test.max); got != test.want {
				t.Fatalf("ClampLimit() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestPageResolve(t *testing.T) {
	page := Page{Limit: 5000, Cursor: EncodeCursor(40)}
	offset, limit, err := page.Resolve(50, 200)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if offset != 40 || limit != 200 {
		t.Fatalf("resolve gave offset=%d limit=%d, want 40 and 200", offset, limit)
	}

	if _, _, err := (Page{Cursor: "forged"}).Resolve(50, 200); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("error = %v, want ErrInvalidCursor", err)
	}
}

// The next cursor is what tells a caller whether to ask again. Issuing one
// past the end makes the caller loop; withholding one early truncates the
// collection silently.
func TestNewPageInfo(t *testing.T) {
	tests := []struct {
		name                    string
		total, offset, returned int
		wantNext                bool
	}{
		{name: "more remains", total: 10, offset: 0, returned: 5, wantNext: true},
		{name: "exactly exhausted", total: 10, offset: 5, returned: 5},
		{name: "a short final page", total: 10, offset: 8, returned: 2},
		{name: "an empty page ends it", total: 10, offset: 10, returned: 0},
		{name: "an empty collection", total: 0, offset: 0, returned: 0},
		// A short page ends the collection: asking again would return nothing
		// and the caller would loop.
		{name: "a short page below the total", total: 100, offset: 0, returned: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := NewPageInfo(test.total, test.offset, test.returned)
			if info.Total != test.total {
				t.Fatalf("total = %d, want %d", info.Total, test.total)
			}
			if (info.NextCursor != "") != test.wantNext {
				t.Fatalf("next cursor = %q, wantNext = %v", info.NextCursor, test.wantNext)
			}
			if test.wantNext {
				offset, err := DecodeCursor(info.NextCursor)
				if err != nil {
					t.Fatalf("the issued cursor must decode: %v", err)
				}
				if offset != test.offset+test.returned {
					t.Fatalf("next offset = %d, want %d", offset, test.offset+test.returned)
				}
			}
		})
	}
}

// Walking a collection must terminate and visit every item exactly once, for
// any page size — including ones that do not divide the total.
func TestWalkingACollectionTerminates(t *testing.T) {
	for _, total := range []int{0, 1, 5, 10, 17} {
		for _, size := range []int{1, 2, 3, 10, 100} {
			visited, offset, pages := 0, 0, 0
			for {
				pages++
				if pages > total+5 {
					t.Fatalf("total=%d size=%d did not terminate", total, size)
				}
				returned := size
				if offset+returned > total {
					returned = total - offset
				}
				if returned < 0 {
					returned = 0
				}
				visited += returned
				info := NewPageInfo(total, offset, returned)
				if info.NextCursor == "" {
					break
				}
				next, err := DecodeCursor(info.NextCursor)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				offset = next
			}
			if visited != total {
				t.Fatalf("total=%d size=%d visited %d items", total, size, visited)
			}
		}
	}
}
