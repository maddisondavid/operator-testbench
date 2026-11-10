package kubeapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRedundantWatchTimeout(t *testing.T) {
	reg, _ := Builtin("v1/configmaps")
	s := serve(t, []Registration{reg}, DefaultLimits(), nil)
	path := "/api/v1/configmaps"
	_, list := request(t, s, "GET", path, "")
	rv := list["metadata"].(map[string]any)["resourceVersion"].(string)
	for _, q := range []string{
		"timeout=1s", "watch=true&timeout=1s", "watch=false&timeout=1s&timeoutSeconds=1",
		"watch=true&timeout=1.1s&timeoutSeconds=1", "watch=true&timeout=x&timeoutSeconds=1",
		"watch=true&timeout=999999999999999999h&timeoutSeconds=1", "watch=true&timeout=&timeoutSeconds=1",
		"watch=true&timeout=0s&timeoutSeconds=0", "watch=true&timeout=3601s&timeoutSeconds=3601",
		"watch=true&timeout=1s&timeout=1s&timeoutSeconds=1", "watch=true&timeout=1s&timeoutSeconds=1&timeoutSeconds=1",
	} {
		if code, _ := request(t, s, "GET", path+"?resourceVersion="+rv+"&"+q, ""); code != 400 {
			t.Fatal(q, code)
		}
	}
	s.mu.Lock()
	n := len(s.subscriptions)
	s.mu.Unlock()
	if n != 0 {
		t.Fatal("rejected watch subscribed")
	}
	select {
	case <-s.Connected:
		t.Fatal("rejected watch connected")
	default:
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		r := httptest.NewRequest(method, path+"?watch=true&timeout=1s&timeoutSeconds=1", nil)
		if s.queryValid(r, "") {
			t.Fatal(method)
		}
	}
	if s.queryValid(httptest.NewRequest("GET", path+"/named?watch=true&timeout=1s&timeoutSeconds=1", nil), "named") {
		t.Fatal("object timeout")
	}
	for _, alias := range []string{"", "&timeout=1s", "&timeout=1000ms"} {
		start := time.Now()
		res, err := http.Get("http://" + s.Address() + path + "?watch=true&resourceVersion=" + rv + "&timeoutSeconds=1" + alias)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 {
			t.Fatal(res.StatusCode)
		}
		_, err = io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || time.Since(start) < 900*time.Millisecond || time.Since(start) > 3*time.Second {
			t.Fatal("expiry", err, time.Since(start))
		}
	}
}
