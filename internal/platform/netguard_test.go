package platform

import (
	"net/http/httptest"
	"testing"
)

func TestIsLocalRequest(t *testing.T) {
	cases := []struct {
		remote string
		want   bool
	}{
		{"127.0.0.1:1234", true},
		{"127.8.9.10:80", true},
		{"[::1]:443", true},
		{"localhost:8080", true},
		{"192.0.2.1:1234", false},
		{"[2001:db8::1]:443", false},
		{"not-a-host-port", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.remote
		if got := IsLocalRequest(r); got != tc.want {
			t.Errorf("IsLocalRequest(%q) = %v, want %v", tc.remote, got, tc.want)
		}
	}
}
