package config

import "testing"

func TestSafePath(t *testing.T) {
	good := []string{"./data/ircapybara.db", "./data/tls/server.crt", "/data/ircapybara.db"}
	bad := []string{"data/ircapybara.db", "../data/ircapybara.db", "./data/../escape.db", "/data/../escape.db"}
	for _, p := range good {
		if !safePath(p) {
			t.Errorf("expected safe path %q", p)
		}
	}
	for _, p := range bad {
		if safePath(p) {
			t.Errorf("expected unsafe path %q", p)
		}
	}
}
