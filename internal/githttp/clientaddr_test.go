package githttp

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

const traefikAddr = "172.18.0.2"

func traefikAt(addr string, ok bool) func(context.Context) (netip.Addr, bool) {
	a := netip.MustParseAddr(addr)
	return func(context.Context) (netip.Addr, bool) { return a, ok }
}

func mustTrusted(t *testing.T, entries ...string) []netip.Prefix {
	t.Helper()
	p, err := ParseTrusted(entries)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func request(remote string, xff ...string) *http.Request {
	r := &http.Request{RemoteAddr: remote, Header: http.Header{}}
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestClientAddr(t *testing.T) {
	const T = traefikAddr + ":4000"
	list := func() []netip.Prefix { return mustTrusted(t, "192.0.2.0/24") }
	cases := []struct {
		name    string
		p       Proxies
		remote  string
		xff     []string
		want    string
		unknown bool
	}{
		{"not traefik: its own address, header ignored", Proxies{}, "198.51.100.7:5000", []string{"1.2.3.4"}, "198.51.100.7", false},
		{"unset: the last entry", Proxies{}, T, []string{"1.2.3.4, 203.0.113.9"}, "203.0.113.9", false},
		{"unset, no header: traefik's address", Proxies{}, T, nil, "172.18.0.2", false},
		{"list: first outside from the right", Proxies{Trusted: list()}, T, []string{"203.0.113.9, 192.0.2.10"}, "203.0.113.9", false},
		{"list: forged entry left of the real one", Proxies{Trusted: list()}, T, []string{"6.6.6.6, 203.0.113.9, 192.0.2.10, 192.0.2.11"}, "203.0.113.9", false},
		{"list: all trusted", Proxies{Trusted: list()}, T, []string{"192.0.2.10, 192.0.2.11"}, "192.0.2.10", false},
		{"list: peer not trusted", Proxies{Trusted: list()}, T, []string{"6.6.6.6, 203.0.113.9"}, "203.0.113.9", false},
		{"all: the entry before the last", Proxies{TrustAll: true}, T, []string{"203.0.113.9, 198.51.100.1"}, "203.0.113.9", false},
		{"all: one entry", Proxies{TrustAll: true}, T, []string{"198.51.100.1"}, "198.51.100.1", false},
		{"entry with a port", Proxies{}, T, []string{"1.2.3.4, 203.0.113.9:4711"}, "203.0.113.9", false},
		{"bracketed v6 with a port", Proxies{}, T, []string{"1.2.3.4, [2001:db8::1]:4711"}, "2001:db8::1", false},
		{"garbage ends the walk at the last good entry", Proxies{Trusted: list()}, T, []string{"203.0.113.9, garbage, 192.0.2.10"}, "192.0.2.10", false},
		{"traefik unknown: no header believed", Proxies{}, T, []string{"203.0.113.9"}, "172.18.0.2", true},
		{"ipv4-mapped peer is compared unmapped, so it is traefik", Proxies{}, "[::ffff:172.18.0.2]:9", []string{"203.0.113.9"}, "203.0.113.9", false},
		{"several header lines read as one list", Proxies{Trusted: list()}, T, []string{"6.6.6.6", "203.0.113.9", "192.0.2.10"}, "203.0.113.9", false},
		{"ipv4-mapped entry is unmapped", Proxies{}, T, []string{"[::ffff:203.0.113.9]:1"}, "203.0.113.9", false},
		{"zone is dropped", Proxies{}, T, []string{"fe80::1%eth0"}, "fe80::1", false},
		{"bracketed without a port", Proxies{}, T, []string{"[2001:db8::2]"}, "2001:db8::2", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.p.Traefik = traefikAt(traefikAddr, !c.unknown)
			got := c.p.ClientAddr(request(c.remote, c.xff...))
			if got != netip.MustParseAddr(c.want) {
				t.Fatalf("ClientAddr = %v; want %v", got, c.want)
			}
		})
	}
}

// A request from anything but Traefik can't choose its address, whatever the
// trust settings say.
func TestClientAddrIgnoresHeadersFromOtherPeers(t *testing.T) {
	for _, p := range []Proxies{
		{},
		{TrustAll: true},
		{Trusted: mustTrusted(t, "0.0.0.0/0", "::/0")},
	} {
		p.Traefik = traefikAt(traefikAddr, true)
		got := p.ClientAddr(request("172.18.0.9:1234", "9.9.9.9", "8.8.8.8"))
		if got != netip.MustParseAddr("172.18.0.9") {
			t.Fatalf("an app container chose its address: %v", got)
		}
	}
	// No Traefik func at all behaves as unknown.
	p := Proxies{}
	if got := p.ClientAddr(request(traefikAddr+":1", "9.9.9.9")); got != netip.MustParseAddr(traefikAddr) {
		t.Fatalf("with no Traefik function, got %v", got)
	}
}

func TestClientAddrGarbage(t *testing.T) {
	p := Proxies{Traefik: traefikAt(traefikAddr, true)}
	long := strings.Repeat("1.2.3.4, ", 10000) + "203.0.113.9"
	for _, xff := range []string{"", " ", ",", ",,,", "1.2.3.4 5.6.7.8", "[", "[]", "[::1", "[::1]:", "::1]", "fe80::1%", "%", "1.2.3.4:", ":80", long, "\x00"} {
		for _, remote := range []string{"", "garbage", T(), "[fe80::1%eth0]:5"} {
			got := p.ClientAddr(request(remote, xff))
			if got.Zone() != "" {
				t.Fatalf("zone in result %v (xff %q)", got, xff)
			}
		}
	}
	if got := p.ClientAddr(request(traefikAddr+":1", long)); got != netip.MustParseAddr("203.0.113.9") {
		t.Fatalf("long header: %v", got)
	}
	// Nothing parseable at all gives the zero address, which callers can test.
	if got := p.ClientAddr(request("garbage")); got.IsValid() {
		t.Fatalf("garbage peer gave %v", got)
	}
}

func T() string { return traefikAddr + ":1" }

func TestParseTrusted(t *testing.T) {
	got, err := ParseTrusted([]string{"192.0.2.7", "2001:db8::/32", " 10.0.0.0/8 "})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"192.0.2.7/32", "2001:db8::/32", "10.0.0.0/8"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("got %v; want %v", got, want)
		}
	}
	if _, err := ParseTrusted([]string{"nonsense"}); err == nil {
		t.Fatal("nonsense accepted")
	}
	if _, err := ParseTrusted([]string{"1.2.3.4/99"}); err == nil {
		t.Fatal("bad prefix accepted")
	}
}
