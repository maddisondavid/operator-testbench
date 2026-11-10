package scenario

import (
	"bytes"
	"encoding/json"
	"gopkg.in/yaml.v3"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var identifier = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,62}$`)
var numberSyntax = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

func parse(data []byte, path string) (map[string]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if e := dec.Decode(&root); e != nil {
		return nil, invalid(path)
	}
	var extra yaml.Node
	if dec.Decode(&extra) != io.EOF {
		return nil, invalid(path)
	}
	count := 0
	var convert func(*yaml.Node, int) (any, error)
	convert = func(n *yaml.Node, depth int) (any, error) {
		count++
		bad := func() (any, error) {
			return nil, &Error{Reason: "invalid_input", Path: path, Line: n.Line, Column: n.Column, Invalid: true}
		}
		if count > 100000 || depth > 64 || n.Anchor != "" || n.Kind == yaml.AliasNode || n.Style&yaml.TaggedStyle != 0 {
			return bad()
		}
		switch n.Kind {
		case yaml.DocumentNode:
			if len(n.Content) != 1 {
				return bad()
			}
			return convert(n.Content[0], depth)
		case yaml.MappingNode:
			if n.Tag != "!!map" {
				return bad()
			}
			m := map[string]any{}
			for i := 0; i < len(n.Content); i += 2 {
				k := n.Content[i]
				if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || k.Value == "<<" || k.Anchor != "" || k.Style&yaml.TaggedStyle != 0 {
					return bad()
				}
				if _, ok := m[k.Value]; ok {
					return bad()
				}
				count++
				v, e := convert(n.Content[i+1], depth+1)
				if e != nil {
					return nil, e
				}
				m[k.Value] = v
			}
			return m, nil
		case yaml.SequenceNode:
			if n.Tag != "!!seq" {
				return bad()
			}
			a := []any{}
			for _, v := range n.Content {
				x, e := convert(v, depth+1)
				if e != nil {
					return nil, e
				}
				a = append(a, x)
			}
			return a, nil
		case yaml.ScalarNode:
			// yaml.v3 falls back to !!str for decimal overflow. Plain JSON
			// numeric syntax, not float64-based tag resolution, determines type.
			if n.Style == 0 && numberSyntax.MatchString(n.Value) {
				if len(n.Value) > 1024 {
					return bad()
				}
				_, exp, has := strings.Cut(strings.ToLower(n.Value), "e")
				if has {
					v, e := strconv.Atoi(exp)
					if e != nil || v > 10000 || v < -10000 {
						return bad()
					}
				}
				return json.Number(n.Value), nil
			}
			switch n.Tag {
			case "!!str":
				return n.Value, nil
			case "!!null":
				if n.Value != "null" {
					return bad()
				}
				return nil, nil
			case "!!bool":
				if n.Value == "true" {
					return true, nil
				}
				if n.Value == "false" {
					return false, nil
				}
				return bad()
			case "!!int", "!!float":
				if len(n.Value) > 1024 || !numberSyntax.MatchString(n.Value) {
					return bad()
				}
				_, exp, ok := strings.Cut(strings.ToLower(n.Value), "e")
				if ok {
					v, e := strconv.Atoi(exp)
					if e != nil || v > 10000 || v < -10000 {
						return bad()
					}
				}
				return json.Number(n.Value), nil
			}
		}
		return bad()
	}
	v, e := convert(&root, 0)
	if e != nil {
		return nil, e
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, invalid(path)
	}
	return m, nil
}
func keys(m map[string]any, allowed ...string) bool {
	for k := range m {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func str(v any) string            { s, _ := v.(string); return s }
func duration(v any, def, min, max time.Duration) (time.Duration, bool) {
	if v == nil {
		return 0, false
	}
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	d, e := time.ParseDuration(s)
	return d, e == nil && d >= min && d <= max
}
func validPointer(p string) bool {
	if len(p) > 1024 || p != "" && !strings.HasPrefix(p, "/") {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '~' {
			i++
			if i >= len(p) || (p[i] != '0' && p[i] != '1') {
				return false
			}
		}
	}
	return true
}

func durationField(m map[string]any, key string, def, min, max time.Duration) (time.Duration, bool) {
	v, present := m[key]
	if !present {
		return def, true
	}
	return duration(v, def, min, max)
}
