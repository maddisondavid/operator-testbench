package scenario

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
)

func canonicalNumber(n json.Number) string {
	text := strings.ToLower(string(n))
	mantissa, exponent, has := strings.Cut(text, "e")
	exp := new(big.Int)
	if has {
		exp.SetString(exponent, 10)
	}
	sign := ""
	if strings.HasPrefix(mantissa, "-") {
		sign = "-"
		mantissa = mantissa[1:]
	}
	if point := strings.IndexByte(mantissa, '.'); point >= 0 {
		exp.Sub(exp, big.NewInt(int64(len(mantissa)-point-1)))
		mantissa = strings.ReplaceAll(mantissa, ".", "")
	}
	mantissa = strings.TrimLeft(mantissa, "0")
	if mantissa == "" {
		return "0"
	}
	trimmed := strings.TrimRight(mantissa, "0")
	exp.Add(exp, big.NewInt(int64(len(mantissa)-len(trimmed))))
	return sign + trimmed + "e" + exp.String()
}
func equalJSON(a, b any) bool {
	switch a := a.(type) {
	case nil:
		return b == nil
	case string:
		v, ok := b.(string)
		return ok && a == v
	case bool:
		v, ok := b.(bool)
		return ok && a == v
	case json.Number:
		v, ok := b.(json.Number)
		return ok && canonicalNumber(a) == canonicalNumber(v)
	case []any:
		v, ok := b.([]any)
		if !ok || len(a) != len(v) {
			return false
		}
		for i := range a {
			if !equalJSON(a[i], v[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		v, ok := b.(map[string]any)
		if !ok || len(a) != len(v) {
			return false
		}
		for k, x := range a {
			y, exists := v[k]
			if !exists || !equalJSON(x, y) {
				return false
			}
		}
		return true
	}
	return false
}

func pointer(value any, p string) (any, bool) {
	if p == "" {
		return value, true
	}
	for _, part := range strings.Split(p[1:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch v := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = v[part]
			if !ok {
				return nil, false
			}
		case []any:
			if part == "" || len(part) > 1 && part[0] == '0' {
				return nil, false
			}
			for _, c := range part {
				if c < '0' || c > '9' {
					return nil, false
				}
			}
			n, e := strconv.Atoi(part)
			if e != nil || n < 0 || n >= len(v) {
				return nil, false
			}
			value = v[n]
		default:
			return nil, false
		}
	}
	return value, true
}
