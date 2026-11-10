// Package reportview validates the existing persisted report format for read-only
// display. It never mutates artifacts or claims that a report proves process exit.
package reportview

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"operator-dynamo/internal/contracts"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

//go:embed schema.json
var schemaBytes []byte
var schema map[string]any

func init() { json.Unmarshal(schemaBytes, &schema) }

var identifier = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,62}$`)
var hash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func invalid() error { return contracts.ReadFailure(422, "invalid_report") }

type parser struct {
	ctx         context.Context
	dec         *json.Decoder
	nodes, cost int
}

func (p *parser) value(depth int) (any, error) {
	if e := p.ctx.Err(); e != nil {
		return nil, e
	}
	p.nodes++
	p.cost += 64
	if depth > 64 || p.nodes > 500000 {
		return nil, invalid()
	}
	if p.cost > 20<<20 {
		return nil, contracts.ReadFailure(413, "representation_too_large")
	}
	token, e := p.dec.Token()
	if e != nil {
		return nil, invalid()
	}
	if d, ok := token.(json.Delim); ok {
		switch d {
		case '{':
			m := map[string]any{}
			p.cost += 256
			for p.dec.More() {
				k, e := p.dec.Token()
				if e != nil {
					return nil, invalid()
				}
				key, ok := k.(string)
				if !ok {
					return nil, invalid()
				}
				if _, found := m[key]; found {
					return nil, invalid()
				}
				p.cost += len(key) + 64
				v, e := p.value(depth + 1)
				if e != nil {
					return nil, e
				}
				m[key] = v
			}
			end, e := p.dec.Token()
			if e != nil || end != json.Delim('}') {
				return nil, invalid()
			}
			return m, nil
		case '[':
			a := []any{}
			for p.dec.More() {
				v, e := p.value(depth + 1)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			end, e := p.dec.Token()
			if e != nil || end != json.Delim(']') {
				return nil, invalid()
			}
			return a, nil
		default:
			return nil, invalid()
		}
	}
	switch v := token.(type) {
	case string:
		p.cost += len(v)
	case json.Number:
		p.cost += len(v)
		if _, ok := integer(v); !ok {
			return nil, invalid()
		}
	case bool, nil:
	default:
		return nil, invalid()
	}
	return token, nil
}

// integer compares mathematical values without expanding exponent strings.
// Only the at-most-19 significant integral digits are ever materialized.
func integer(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	s := n.String()
	negative := strings.HasPrefix(s, "-")
	if negative {
		s = s[1:]
	}
	mantissa, exponent := s, ""
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa, exponent = s[:i], s[i+1:]
	}
	digits, first, last, point := 0, -1, -1, -1
	for _, c := range mantissa {
		if c == '.' {
			if point >= 0 {
				return 0, false
			}
			point = digits
			continue
		}
		if c < '0' || c > '9' {
			return 0, false
		}
		if c != '0' {
			if first < 0 {
				first = digits
			}
			last = digits
		}
		digits++
	}
	if digits == 0 {
		return 0, false
	}
	if first < 0 {
		return 0, true
	}
	if negative {
		return 0, false
	}
	exp, sign := 0, 1
	if len(exponent) > 0 {
		if exponent[0] == '-' {
			sign = -1
			exponent = exponent[1:]
		} else if exponent[0] == '+' {
			exponent = exponent[1:]
		}
		for _, c := range exponent {
			if c < '0' || c > '9' {
				return 0, false
			}
			if exp <= len(s)+20 {
				exp = exp*10 + int(c-'0')
			}
		}
	}
	exp *= sign
	fraction := 0
	if point >= 0 {
		fraction = digits - point
	}
	shift := exp - fraction + (digits - last - 1)
	significant := last - first + 1
	if shift < 0 || significant+shift > 19 {
		return 0, false
	}
	var buf [19]byte
	index, count := 0, 0
	for _, c := range mantissa {
		if c == '.' {
			continue
		}
		if index >= first && index <= last {
			buf[count] = byte(c)
			count++
		}
		index++
	}
	for i := 0; i < shift; i++ {
		buf[count] = '0'
		count++
	}
	value, e := strconv.ParseInt(string(buf[:count]), 10, 64)
	return value, e == nil
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func text(v any) string           { s, _ := v.(string); return s }
func list(v any) []any            { a, _ := v.([]any); return a }
func validate(v any, s map[string]any) bool {
	if c, ok := s["const"]; ok && !reflect.DeepEqual(v, c) {
		return false
	}
	if enums, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range enums {
			if reflect.DeepEqual(v, e) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	switch declared := s["type"].(type) {
	case nil:
		// Const/enum-only schemas have already been checked above.
		return true
	case string:
		return validateType(v, s, declared)
	case []any:
		// A union admits only an explicitly declared type. Each candidate
		// retains the original schema constraints, including array items.
		for _, candidate := range declared {
			kind, ok := candidate.(string)
			if ok && validateType(v, s, kind) {
				return true
			}
		}
	}
	return false
}
func validateType(v any, s map[string]any, kind string) bool {
	switch kind {
	case "null":
		return v == nil
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		props := object(s["properties"])
		for _, key := range list(s["required"]) {
			if _, ok := m[text(key)]; !ok {
				return false
			}
		}
		for k, value := range m {
			property, ok := props[k]
			if !ok {
				return false
			}
			if !validate(value, object(property)) {
				return false
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			return false
		}
		if max, ok := s["maxItems"].(float64); ok && len(a) > int(max) {
			return false
		}
		for _, v := range a {
			if !validate(v, object(s["items"])) {
				return false
			}
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return false
		}
		if min, ok := s["minLength"].(float64); ok && len(str) < int(min) {
			return false
		}
		if max, ok := s["maxLength"].(float64); ok && len(str) > int(max) {
			return false
		}
		if s["format"] == "date-time" && !timestamp(str) {
			return false
		}
	case "integer":
		if _, ok := integer(v); !ok {
			return false
		}
	default:
		return false
	}
	return true
}
func timestamp(s string) bool {
	v, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return false
	}
	_, off := v.Zone()
	return off == 0
}
func path(v any) bool {
	s, ok := v.(string)
	return ok && filepath.IsLocal(s) && !strings.ContainsAny(s, "\x00\\")
}
func num(v any) int64 { n, _ := integer(v); return n }
func Validate(ctx context.Context, data []byte, id string) (map[string]any, error) {
	if len(data) > 16<<20 {
		return nil, contracts.ReadFailure(413, "representation_too_large")
	}
	if !utf8.Valid(data) {
		return nil, invalid()
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	p := parser{ctx: ctx, dec: d}
	v, e := p.value(0)
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, invalid()
	}
	if p.cost > 20<<20 {
		return nil, contracts.ReadFailure(413, "representation_too_large")
	}
	if !validate(v, schema) {
		return nil, invalid()
	}
	m := object(v)
	if text(m["runId"]) != id || !contracts.ValidID(id) || !path(m["selectionPath"]) {
		return nil, invalid()
	}
	for _, item := range list(m["inputs"]) {
		in := object(item)
		if !path(in["path"]) || !hash.MatchString(text(in["sha256"])) {
			return nil, invalid()
		}
	}
	status := text(m["status"])
	terminal := status != "running"
	_, hasEnd := m["endedAt"]
	_, hasExit := m["exitCode"]
	if terminal {
		codes := map[string]int64{"passed": 0, "failed": 1, "cancelled": 1, "error": 3}
		code, ok := codes[status]
		if !ok || !hasEnd || !hasExit || num(m["exitCode"]) != code {
			return nil, invalid()
		}
	} else if hasEnd || hasExit {
		return nil, invalid()
	}
	scenarios := list(m["scenarios"])
	if len(scenarios) < 1 || len(scenarios) > 100 {
		return nil, invalid()
	}
	counts := map[string]int64{"total": int64(len(scenarios)), "pending": 0, "running": 0, "passed": 0, "failed": 0, "error": 0, "cancelled": 0, "skipped": 0}
	seen := map[string]bool{}
	cleanupError := false
	for _, value := range scenarios {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		s := object(value)
		state := text(s["status"])
		counts[state]++
		source := text(s["sourcePath"])
		if !path(source) || seen[source] || !identifier.MatchString(text(s["name"])) {
			return nil, invalid()
		}
		seen[source] = true
		if len(list(s["suitePaths"])) > 32 {
			return nil, invalid()
		}
		for _, v := range list(s["suitePaths"]) {
			if !path(v) {
				return nil, invalid()
			}
		}
		if sid, ok := s["sessionId"]; ok && !contracts.ValidID(text(sid)) {
			return nil, invalid()
		}
		if art, ok := s["artifactPath"]; ok && !path(art) {
			return nil, invalid()
		}
		if terminal && (state == "pending" || state == "running") {
			return nil, invalid()
		}
		if (state == "pending" || state == "skipped") && s["sessionId"] != nil {
			return nil, invalid()
		}
		steps := list(s["steps"])
		if len(steps) < 1 || len(steps) > 100 {
			return nil, invalid()
		}
		ids := map[string]bool{}
		for _, v := range steps {
			step := object(v)
			sid := text(step["id"])
			out := text(step["outcome"])
			if !identifier.MatchString(sid) || ids[sid] {
				return nil, invalid()
			}
			ids[sid] = true
			if state == "passed" && out != "passed" || state == "pending" && out != "pending" || state == "skipped" && out != "skipped" {
				return nil, invalid()
			}
			if state != "pending" && state != "running" && (out == "pending" || out == "running") {
				return nil, invalid()
			}
			if out == "skipped" && num(step["durationNs"]) != 0 {
				return nil, invalid()
			}
		}
		cleanup := object(s["cleanup"])
		out := text(cleanup["outcome"])
		if out == "error" {
			cleanupError = true
			if text(cleanup["reasonCode"]) == "" {
				return nil, invalid()
			}
		}
		if out == "notStarted" && num(cleanup["durationNs"]) != 0 {
			return nil, invalid()
		}
		if status == "passed" && (state != "passed" || out != "passed") {
			return nil, invalid()
		}
	}
	if counts["running"] > 1 {
		return nil, invalid()
	}
	if status == "failed" && (counts["failed"] == 0 || counts["error"] > 0 || cleanupError) {
		return nil, invalid()
	}
	for key, n := range counts {
		if num(object(m["counts"])[key]) != n {
			return nil, invalid()
		}
	}
	return stringsView(m).(map[string]any), nil
}
func stringsView(v any) any {
	switch x := v.(type) {
	case json.Number:
		n, _ := integer(x)
		return strconv.FormatInt(n, 10)
	case map[string]any:
		for k, v := range x {
			x[k] = stringsView(v)
		}
	case []any:
		for i, v := range x {
			x[i] = stringsView(v)
		}
	}
	return v
}
