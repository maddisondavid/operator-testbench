package uiaddress

import "testing"

func TestExplicitPublishedOrigin(t *testing.T) {
	for _, s := range []string{"http://198.51.100.10:18080", "http://[::1]:18080/", "http://localhost:8080"} {
		if _, e := Origin(s); e != nil {
			t.Fatal(s, e)
		}
	}
	for _, s := range []string{"http://0.0.0.0:80", "http://[::]:80", "http://example.test:80", "http://127.0.0.1:0", "https://127.0.0.1:80", "http://u@127.0.0.1:80", "http://127.0.0.1:80?", "http://127.0.0.1:80#", "http://127.0.0.1:80/ui/", "http://[fe80::1%25eth0]:80", "http://127.0.0.1"} {
		if _, e := Origin(s); e == nil {
			t.Fatal(s)
		}
	}
}
