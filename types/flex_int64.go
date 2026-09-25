package types

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// FlexInt64 carries a 64-bit identifier across a JSON boundary without losing
// precision in JavaScript clients.
//
// Snowflake IDs issued by common/idgen exceed 2^53, which a browser Number
// rounds silently. FlexInt64 therefore decodes both a JSON number and a JSON
// string, and always encodes as a JSON string so a client that keeps the raw
// text can round-trip the exact value.
//
// Use it for identifiers in request and response DTOs. A stored column should
// stay int64 with a `json:",string"` tag instead.
type FlexInt64 int64

// NewFlexInt64 wraps a plain int64 identifier.
func NewFlexInt64(value int64) FlexInt64 {
	return FlexInt64(value)
}

// Int64 returns the underlying identifier.
func (f FlexInt64) Int64() int64 {
	return int64(f)
}

// String renders the identifier in decimal, matching its JSON representation.
func (f FlexInt64) String() string {
	return strconv.FormatInt(int64(f), 10)
}

// MarshalJSON always emits a quoted decimal string.
func (f FlexInt64) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('"')
	buf.WriteString(strconv.FormatInt(int64(f), 10))
	buf.WriteByte('"')
	return buf.Bytes(), nil
}

// UnmarshalJSON accepts a JSON number, a JSON string holding a decimal
// integer, or null. A fractional or out-of-range value is rejected rather
// than truncated, because a silently rounded identifier points at a different
// row.
func (f *FlexInt64) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*f = 0
		return nil
	}

	var raw string
	if trimmed[0] == '"' {
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return err
		}
	} else {
		raw = string(trimmed)
	}

	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return &strconv.NumError{Func: "UnmarshalJSON", Num: raw, Err: strconv.ErrSyntax}
	}
	*f = FlexInt64(value)
	return nil
}
