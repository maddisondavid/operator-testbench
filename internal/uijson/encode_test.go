package uijson

import (
	"context"
	"encoding/json"
	"errors"
	"operator-dynamo/internal/contracts"
	"strings"
	"testing"
)

func TestEncodedLimitAndEscapes(t *testing.T) {
	source := []byte("{\"precise\":9007199254740993,\"s\":\"<script>\\n\"}")
	b, e := Encode(context.Background(), map[string]any{"raw": RawText(source)})
	if e != nil {
		t.Fatal(e)
	}
	var v map[string]string
	if e = json.Unmarshal(b, &v); e != nil || v["raw"] != string(source) {
		t.Fatal(string(b), e)
	}
	_, e = Encode(context.Background(), map[string]any{"raw": RawText(strings.Repeat("\x00", 6<<20))})
	var r *contracts.ReadError
	if !errors.As(e, &r) || r.Status != 413 {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = Encode(ctx, map[string]any{"x": "ok"}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
