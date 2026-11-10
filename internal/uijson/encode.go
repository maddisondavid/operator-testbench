// Package uijson encodes an envelope into a single bounded buffer without a
// second full marshaling allocation. RawText is JSON text displayed as a string.
package uijson

import (
	"context"
	"encoding/json"
	"operator-dynamo/internal/contracts"
	"sort"
	"strconv"
)

type RawText []byte
type encoder struct {
	ctx context.Context
	b   []byte
	err error
}

func (e *encoder) add(b ...byte) {
	if e.err != nil {
		return
	}
	if len(e.b)+len(b) > 32<<20 {
		e.err = contracts.ReadFailure(413, "representation_too_large")
		return
	}
	if len(e.b)&32767 < len(b) {
		if err := e.ctx.Err(); err != nil {
			e.err = err
			return
		}
	}
	if len(e.b)+len(b) > cap(e.b) {
		capacity := cap(e.b) * 2
		if capacity < 4096 {
			capacity = 4096
		}
		if capacity > 8<<20 {
			capacity = ((len(e.b) + len(b) + (8 << 20) - 1) / (8 << 20)) * (8 << 20)
		}
		if capacity < len(e.b)+len(b) {
			capacity = len(e.b) + len(b)
		}
		if capacity > 32<<20 {
			capacity = 32 << 20
		}
		grown := make([]byte, len(e.b), capacity)
		copy(grown, e.b)
		e.b = grown
	}
	e.b = append(e.b, b...)
}
func (e *encoder) str(b []byte) {
	e.add('"')
	start := 0
	for i, c := range b {
		if c == '"' || c == '\\' || c < 32 {
			e.add(b[start:i]...)
			if c == '"' || c == '\\' {
				e.add('\\', c)
			} else {
				const hex = "0123456789abcdef"
				e.add('\\', 'u', '0', '0', hex[c>>4], hex[c&15])
			}
			start = i + 1
		}
		if i%32768 == 0 {
			if err := e.ctx.Err(); err != nil {
				e.err = err
				return
			}
		}
	}
	e.add(b[start:]...)
	e.add('"')
}

// String chunks are copied in small pieces, never duplicated as a full []byte.
func (e *encoder) text(s string) {
	e.add('"')
	var buf [4096]byte
	n := 0
	flush := func() { e.add(buf[:n]...); n = 0 }
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' || c < 32 {
			flush()
			if c == '"' || c == '\\' {
				e.add('\\', c)
			} else {
				const hex = "0123456789abcdef"
				e.add('\\', 'u', '0', '0', hex[c>>4], hex[c&15])
			}
		} else {
			buf[n] = c
			n++
			if n == len(buf) {
				flush()
			}
		}
		if e.err != nil {
			return
		}
	}
	flush()
	e.add('"')
}
func (e *encoder) value(v any) {
	if e.err != nil {
		return
	}
	switch x := v.(type) {
	case nil:
		e.add([]byte("null")...)
	case string:
		e.text(x)
	case RawText:
		e.str(x)
	case json.RawMessage:
		e.add(x...)
	case bool:
		if x {
			e.add([]byte("true")...)
		} else {
			e.add([]byte("false")...)
		}
	case map[string]any:
		e.add('{')
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			if i > 0 {
				e.add(',')
			}
			e.str([]byte(k))
			e.add(':')
			e.value(x[k])
		}
		e.add('}')
	case []any:
		e.add('[')
		for i, v := range x {
			if i > 0 {
				e.add(',')
			}
			e.value(v)
		}
		e.add(']')
	case int:
		e.add([]byte(strconv.Itoa(x))...)
	default:
		// Other values are bounded DTOs (catalog, summaries, one event page), never
		// arbitrary report/object graphs. Their encoding is capped independently.
		b, err := json.Marshal(x)
		if err != nil {
			e.err = err
			return
		}
		if len(b) > 4<<20 {
			e.err = contracts.ReadFailure(413, "representation_too_large")
			return
		}
		e.add(b...)
	}
}
func Encode(ctx context.Context, v map[string]any) ([]byte, error) {
	e := encoder{ctx: ctx}
	e.value(v)
	if e.err == nil {
		e.err = ctx.Err()
	}
	return e.b, e.err
}
