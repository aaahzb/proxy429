package main

// Unit tests for proxycheck's pure parsers: scutil output, reg query output, NO_PROXY/ProxyOverride
// loopback coverage, and detectHijackRisk's gate (no probing, no alarm when responses_listen is unset).

import "testing"

func TestNoProxyCoversLoopback(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"example.com", false},
		{"localhost,127.0.0.1,::1", true},
		{" 127.0.0.1 , foo.com", true}, // whitespace tolerated
		{"foo.com,localhost", true},    // localhost counts as loopback coverage
		{"*", true},                    // wildcard bypass
		{"127.0.0.0/8", false},         // CIDR form is not what our setup writes — not accepted (err toward warning)
		{"10.10.0.0/16,::1", true},     // ::1 alone also counts
	}
	for _, c := range cases {
		if got := noProxyCoversLoopback(c.in); got != c.want {
			t.Errorf("noProxyCoversLoopback(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestScutilProxyEnabled(t *testing.T) {
	on := `<dictionary> {
  ExceptionsList : <array> {
    0 : 127.0.0.1
  }
  HTTPEnable : 1
  HTTPPort : 8118
  HTTPProxy : 127.0.0.1
  HTTPSEnable : 1
  HTTPSPort : 8118
  HTTPSProxy : 127.0.0.1
}`
	if en, desc := scutilProxyEnabled(on); !en || desc != "127.0.0.1:8118" {
		t.Errorf("proxy on should be true+address, got %v %q", en, desc)
	}

	off := `<dictionary> {
  FTPPassive : 1
  HTTPEnable : 0
  HTTPSEnable : 0
}`
	if en, _ := scutilProxyEnabled(off); en {
		t.Error("all off should be false")
	}

	socksOnly := `<dictionary> {
  SOCKSEnable : 1
  SOCKSPort : 1080
  SOCKSProxy : 127.0.0.1
}`
	if en, _ := scutilProxyEnabled(socksOnly); en {
		t.Error("SOCKS-only must not count (the one-line script only watches HTTP/HTTPS)")
	}
}

func TestRegQueryValueData(t *testing.T) {
	out := "\r\nHKEY_CURRENT_USER\\Software\\Microsoft\\Windows\\CurrentVersion\\Internet Settings\r\n    ProxyEnable    REG_DWORD    0x1\r\n    ProxyServer    REG_SZ    127.0.0.1:8118\r\n"
	if got := regQueryValueData(out, "ProxyEnable"); got != "0x1" {
		t.Errorf("ProxyEnable=%q want 0x1", got)
	}
	if got := regQueryValueData(out, "ProxyServer"); got != "127.0.0.1:8118" {
		t.Errorf("ProxyServer=%q", got)
	}
	if got := regQueryValueData(out, "NoSuch"); got != "" {
		t.Errorf("missing value should be empty, got %q", got)
	}
	if got := regQueryValueData("ERROR: The system was unable to find the specified registry key or value.", "NO_PROXY"); got != "" {
		t.Errorf("reg error output should be empty, got %q", got)
	}
}

func TestProxyOverrideCoversLoopback(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"*.local;foo.com", false},
		{"127.0.0.1;*.local", true},
		{"localhost;<-loopback>", true},
		{"<-loopback>", true}, // Windows built-in loopback macro
	}
	for _, c := range cases {
		if got := proxyOverrideCoversLoopback(c.in); got != c.want {
			t.Errorf("proxyOverrideCoversLoopback(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

// With responses_listen unset the gate passes quietly (no alarm): no Responses port means no Codex to hijack.
func TestDetectHijackRiskGatedWithoutResponsesListen(t *testing.T) {
	prev := cfg.Load()
	cfg.Store(&Config{})
	defer cfg.Store(prev)
	if risk, _ := detectHijackRisk(); risk {
		t.Error("no alarm expected when responses_listen is empty")
	}
}
