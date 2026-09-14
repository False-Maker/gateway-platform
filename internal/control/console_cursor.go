package control

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// B5.1: cursor pagination for the console's list endpoints.
//
// B5 capped every list with LIMIT and recorded the absence of paging as a gap.
// The cap alone is the problem: a held tray with 900 rows returns the first 500
// and looks exactly like a tray with 500 rows. An operator working a backlog
// therefore cannot tell "this is everything" from "this is where the page
// stopped", and the rows they never see are, by construction, the oldest ones.
//
// Offset paging is not used. These tables are appended to while an operator
// reads them, and OFFSET over a shifting set silently skips and repeats rows --
// for a billing backlog that means a held row can be stepped over entirely.
// A keyset cursor over the query's own sort key cannot: it names the last row
// seen and asks for what comes strictly after it.
//
// The cursor is opaque on purpose. It is base64 over "<sort-key>:<id>" with no
// signature, because it encodes nothing secret -- only a position the caller
// just read. Opacity here buys the freedom to change the sort key later
// without breaking a caller who stored one, not confidentiality, and claiming
// otherwise would be misleading.

// consoleCursor is a decoded position: the tie-broken id plus the sort value it
// accompanied. Every paged query orders by (sort key, id) so the pair is unique
// even when many rows share a timestamp.
type consoleCursor struct {
	// Sort is the primary sort value, rendered as a string (RFC3339Nano for
	// timestamps). Empty when the query sorts by id alone.
	Sort string
	// ID is the tie-breaking primary key.
	ID int64
}

var errBadCursor = errors.New("cursor is not one this endpoint issued")

// encodeCursor renders a position for the next request.
func encodeCursor(sort string, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(sort + "\x00" + strconv.FormatInt(id, 10)))
}

// parseCursor decodes a cursor. An empty string means "start at the beginning",
// which is the only reason a caller ever omits it.
//
// A malformed cursor is an error rather than a silent restart from the top. A
// caller paging through a backlog who is quietly sent back to row one would
// loop forever over the same page and never learn why.
func parseCursor(raw string) (consoleCursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return consoleCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return consoleCursor{}, errBadCursor
	}
	parts := strings.SplitN(string(decoded), "\x00", 2)
	if len(parts) != 2 {
		return consoleCursor{}, errBadCursor
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return consoleCursor{}, errBadCursor
	}
	return consoleCursor{Sort: parts[0], ID: id}, nil
}

// pageResponse wraps a page of rows.
//
// NextCursor is empty when the page is the last one, and HasMore says the same
// thing as a boolean so a caller does not have to infer it from an empty
// string. Both are present because the failure this whole file exists to
// prevent is a caller mistaking a full page for the end of the data.
func pageResponse(key string, rows any, count int, nextCursor string) map[string]any {
	return map[string]any{
		key:           rows,
		"count":       count,
		"next_cursor": nextCursor,
		"has_more":    nextCursor != "",
	}
}

// fetchLimit is how many rows to ask the database for when the caller wants
// `limit`. One extra row is read and then dropped: its existence is what
// proves there is a next page, and asking for it costs one row rather than a
// second COUNT query over the same predicate.
func fetchLimit(limit int) int { return limit + 1 }

// cursorError reports a bad cursor as a 400 with a message that says what to do.
func cursorError(err error) string {
	return fmt.Sprintf("%v; omit the cursor to start from the beginning", err)
}
