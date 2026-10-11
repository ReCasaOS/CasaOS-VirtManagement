package v2

import (
	"net/http/httptest"
	"testing"
)

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		host, origin string
		want         bool
	}{
		{"192.168.10.192", "http://192.168.10.192", true},
		{"casa.local:8443", "https://casa.local:8443", true},
		{"CASA.local", "http://casa.LOCAL", true},
		{"192.168.10.192", "", false},
		{"192.168.10.192", "https://evil.example", false},
		{"192.168.10.192", "http://192.168.10.192:8080", false},
		{"192.168.10.192", "null", false},
	}

	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = tc.host
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}

		if got := SameOrigin(r); got != tc.want {
			t.Errorf("Host %q Origin %q: got %v, want %v", tc.host, tc.origin, got, tc.want)
		}
	}
}
