package kubeapi

import (
	"net/http/httptest"
	"operator-dynamo/internal/contracts"
	"testing"
)

func TestResponseRequestIDIsGeneratedAndJournalLinked(t *testing.T) {
	var corr string
	s := New("", []Registration{widget}, func(o contracts.Observation) error {
		if o.Kind == "api.request" {
			corr = o.CorrelationID
		}
		return nil
	})
	for _, p := range []string{"/apis", "/unknown"} {
		r := httptest.NewRequest("GET", p, nil)
		r.Header.Set("X-Dynamo-Request-ID", "caller-untrusted")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		id := w.Header().Get("X-Dynamo-Request-ID")
		if id == "" || id == "caller-untrusted" || id != corr {
			t.Fatal(id, corr)
		}
	}
}
