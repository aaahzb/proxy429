package platform

import (
	"net"
	"net/http"
	"strings"
)

// IsLocalRequest reports whether the request originates from a loopback address.
func IsLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost" || strings.HasPrefix(host, "127.")
}
