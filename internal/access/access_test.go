package access

import (
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"testing"
)

func TestParse(t *testing.T) {
	cases := map[string]string{
		"":                    "local",
		"local":               "local",
		"LAN":                 "lan",
		"wlan":                "lan",
		"tailscale,lan":       "lan,tailscale",
		" lan , tailscale ":   "lan,tailscale",
		"all":                 "all",
		"0.0.0.0":             "all",
		"lan,all":             "all",
		"192.168.1.50":        "192.168.1.50",
		"127.0.0.1":           "local",
		"tailscale,[fd00::1]": "tailscale,fd00::1",
	}
	for in, want := range cases {
		m, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if got := m.String(); got != want {
			t.Errorf("Parse(%q).String() = %q, want %q", in, got, want)
		}
		if again, _ := Parse(m.String()); again.String() != m.String() {
			t.Errorf("%q does not round-trip", m.String())
		}
	}
	if _, err := Parse("everyone,lna"); err == nil {
		t.Error("a typo must be an error, not silently ignored")
	}
}

// The interfaces of the homeserver this was written on.
var homeserver = []Interface{
	{Name: "lo", Up: true, Loopback: true, Addrs: addrs("127.0.0.1", "::1")},
	{Name: "enp1s0", Up: true, Addrs: addrs("192.168.68.50", "fe80::eaff:1eff:fedf:fa03")},
	{Name: "wlp2s0", Up: true, Addrs: addrs("192.168.68.63")},
	{Name: "tailscale0", Up: true, Addrs: addrs("100.73.243.14", "fd7a:115c:a1e0::f239:f30f")},
	{Name: "br-21905c832abf", Up: true, Addrs: addrs("172.17.0.1")},
	{Name: "docker0", Up: false, Addrs: addrs("10.0.0.1")},
	{Name: "veth5323995", Up: true, Addrs: addrs("fe80::3451:caff:fe5c:72c6")},
	{Name: "vEthernet (WSL)", Up: true, Addrs: addrs("172.29.64.1")},
	{Name: "wan0", Up: true, Addrs: addrs("203.0.113.9")},
}

func addrs(list ...string) []netip.Addr {
	out := make([]netip.Addr, len(list))
	for i, s := range list {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

func endpoints(t *testing.T, spec string) map[string]Kind {
	t.Helper()
	m, err := Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Kind{}
	for _, ep := range m.Endpoints(homeserver) {
		out[ep.Addr.String()] = ep.Kind
	}
	return out
}

func TestEndpoints(t *testing.T) {
	local := map[string]Kind{"127.0.0.1": KindLocal, "::1": KindLocal}
	check := func(spec string, want map[string]Kind) {
		t.Helper()
		got := endpoints(t, spec)
		if len(got) != len(want) {
			t.Errorf("%s: got %v, want %v", spec, got, want)
			return
		}
		for a, k := range want {
			if got[a] != k {
				t.Errorf("%s: %s is %q, want %q (all: %v)", spec, a, got[a], k, got)
			}
		}
	}
	with := func(extra map[string]Kind) map[string]Kind {
		out := map[string]Kind{}
		for k, v := range local {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	check("local", local)
	check("lan", with(map[string]Kind{"192.168.68.50": KindLAN, "192.168.68.63": KindLAN}))
	check("tailscale", with(map[string]Kind{"100.73.243.14": KindTailscale, "fd7a:115c:a1e0::f239:f30f": KindTailscale}))
	check("192.168.68.50", with(map[string]Kind{"192.168.68.50": KindCustom}))
	check("all", map[string]Kind{"0.0.0.0": KindAll, "::": KindAll})
}

func TestReachableOrder(t *testing.T) {
	got := Reachable(homeserver)
	if got[0].Kind != KindLocal || got[1].Kind != KindTailscale {
		t.Fatalf("loopback then Tailscale should lead: %v", got)
	}
	for _, ep := range got {
		if ep.Addr.String() == "172.17.0.1" || ep.Addr.String() == "172.29.64.1" {
			t.Errorf("virtual interface address %s offered as a way in", ep.Addr)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestManagerSwitchesModes(t *testing.T) {
	port := freePort(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })}
	defer srv.Close()
	m := NewManager(port, srv, t.Logf)
	m.Interfaces = func() []Interface { return nil }
	defer m.Close()

	get := func() error {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := m.Apply(Mode{}); err != nil {
		t.Fatal(err)
	}
	if err := get(); err != nil {
		t.Fatalf("local: %v", err)
	}
	// "all" has to give 127.0.0.1 back before 0.0.0.0 can take the port.
	if err := m.Apply(Mode{All: true}); err != nil {
		t.Fatalf("switching to all: %v", err)
	}
	if err := get(); err != nil {
		t.Fatalf("all, via loopback: %v", err)
	}
	if err := m.Apply(Mode{}); err != nil {
		t.Fatalf("switching back: %v", err)
	}
	if err := get(); err != nil {
		t.Fatalf("local again: %v", err)
	}
	if links := m.Links(); len(links) != 1 || links[0].Kind != KindLocal {
		t.Errorf("links = %v", links)
	}
}

func TestManagerReportsBusyPort(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	m := NewManager(l.Addr().(*net.TCPAddr).Port, &http.Server{}, t.Logf)
	if err := m.Apply(Mode{}); err == nil {
		t.Fatal("a taken port on loopback must be an error")
	}
}
