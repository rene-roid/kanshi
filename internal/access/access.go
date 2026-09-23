// Package access decides which addresses the dashboard listens on.
//
// Kanshi has no login, so who can reach it is decided entirely by where it
// listens. Loopback is always on: whatever else is chosen, the machine itself
// can open http://localhost:<port>. On top of that the mode can add the
// machine's Tailscale addresses, its private LAN addresses (wired and Wi-Fi),
// explicit addresses, or every interface at once.
package access

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
)

// Mode is a parsed access setting. The zero value is "local".
type Mode struct {
	LAN       bool
	Tailscale bool
	All       bool
	IPs       []netip.Addr
}

// Parse reads a comma-separated list such as "lan,tailscale". Explicit IP
// addresses may be mixed in. An empty string is "local".
func Parse(spec string) (Mode, error) {
	var m Mode
	for _, tok := range strings.Split(spec, ",") {
		tok = strings.ToLower(strings.TrimSpace(tok))
		switch tok {
		case "", "local", "localhost", "loopback":
		case "lan", "wlan", "wifi", "network":
			m.LAN = true
		case "tailscale", "ts", "tailnet":
			m.Tailscale = true
		case "all", "any", "everywhere", "*", "0.0.0.0", "::", "[::]":
			m.All = true
		default:
			ip, err := netip.ParseAddr(strings.Trim(tok, "[]"))
			if err != nil {
				return Mode{}, fmt.Errorf("unknown access mode %q (want local, lan, tailscale, all or an IP address)", tok)
			}
			ip = ip.Unmap()
			if ip.IsUnspecified() {
				m.All = true
			} else if !ip.IsLoopback() && !contains(m.IPs, ip) {
				m.IPs = append(m.IPs, ip)
			}
		}
	}
	return m, nil
}

// String is the canonical spelling, which Parse reads back unchanged.
func (m Mode) String() string {
	if m.All {
		return "all"
	}
	var parts []string
	if m.LAN {
		parts = append(parts, "lan")
	}
	if m.Tailscale {
		parts = append(parts, "tailscale")
	}
	for _, ip := range m.IPs {
		parts = append(parts, ip.String())
	}
	if len(parts) == 0 {
		return "local"
	}
	return strings.Join(parts, ",")
}

// dynamic reports whether the set of addresses depends on the machine's
// interfaces, and so has to be re-checked as they come and go.
func (m Mode) dynamic() bool {
	return !m.All && (m.LAN || m.Tailscale || len(m.IPs) > 0)
}

// Kind labels an address for the startup banner and the dashboard footer.
type Kind string

const (
	KindLocal     Kind = "local"
	KindLAN       Kind = "lan"
	KindTailscale Kind = "tailscale"
	KindAll       Kind = "all"
	KindCustom    Kind = "custom"
	KindOther     Kind = "other"
)

// Endpoint is one address to listen on.
type Endpoint struct {
	Addr netip.Addr
	Kind Kind
}

// Interface is the part of a network interface that classification needs.
type Interface struct {
	Name     string
	Up       bool
	Loopback bool
	Addrs    []netip.Addr
}

var (
	tailnet4 = netip.MustParsePrefix("100.64.0.0/10")
	tailnet6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
	loop4    = netip.AddrFrom4([4]byte{127, 0, 0, 1})
	loop6    = netip.IPv6Loopback()
)

// IsTailscale reports whether an address is in the ranges Tailscale assigns.
// It is decided by address rather than interface name, because the adapter is
// "tailscale0" on Linux and "Tailscale" on Windows.
func IsTailscale(a netip.Addr) bool {
	return tailnet4.Contains(a) || tailnet6.Contains(a)
}

// isLAN is a private address that another machine on the local network could
// actually connect to: RFC 1918 for IPv4 and unique-local for IPv6.
// Link-local addresses are left out because they are unusable without a zone,
// and public addresses because they are not "the LAN".
func isLAN(a netip.Addr) bool {
	return a.IsPrivate() && !IsTailscale(a)
}

// isVirtual matches interfaces that carry private addresses but are not the
// LAN: Docker and libvirt bridges, container veths, VPN tunnels, and the
// Hyper-V switches Windows creates for WSL and Docker Desktop. Listening there
// would only expose the dashboard to containers and VMs.
func isVirtual(name string) bool {
	n := strings.ToLower(name)
	for _, p := range []string{"docker", "veth", "virbr", "vnet", "cni", "flannel", "cali", "kube", "podman", "lxc", "lxd", "tailscale", "tun", "tap", "wg", "zt", "vethernet", "vmnet"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	// Compose networks are "br-" plus 12 hex digits. A plain "br0" is more
	// likely a real LAN bridge, so it is left alone.
	if strings.HasPrefix(n, "br-") && len(n) == 15 {
		return true
	}
	for _, s := range []string{"virtualbox", "vmware", "hyper-v", "wsl", "loopback pseudo"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// classify says what kind of address a is on the named interface, or false
// when it is neither loopback, LAN nor Tailscale.
func classify(iface Interface, a netip.Addr) (Kind, bool) {
	switch {
	case a.IsLoopback():
		return KindLocal, true
	case IsTailscale(a):
		return KindTailscale, true
	case isVirtual(iface.Name) || a.IsLinkLocalUnicast():
		return "", false
	case isLAN(a):
		return KindLAN, true
	}
	return KindOther, true
}

// Endpoints lists the addresses a mode listens on, given the current
// interfaces.
func (m Mode) Endpoints(ifaces []Interface) []Endpoint {
	if m.All {
		// The wildcards already cover loopback. Binding 127.0.0.1 as well
		// would collide with 0.0.0.0 on the same port.
		return []Endpoint{
			{netip.IPv4Unspecified(), KindAll},
			{netip.IPv6Unspecified(), KindAll},
		}
	}
	out := []Endpoint{{loop4, KindLocal}, {loop6, KindLocal}}
	have := map[netip.Addr]bool{loop4: true, loop6: true}
	add := func(a netip.Addr, k Kind) {
		if !have[a] {
			have[a] = true
			out = append(out, Endpoint{a, k})
		}
	}
	for _, iface := range ifaces {
		if !iface.Up || iface.Loopback {
			continue
		}
		for _, a := range iface.Addrs {
			switch k, ok := classify(iface, a); {
			case !ok:
			case k == KindTailscale && m.Tailscale, k == KindLAN && m.LAN:
				add(a, k)
			}
		}
	}
	for _, ip := range m.IPs {
		add(ip, KindCustom)
	}
	return out
}

// Reachable lists the addresses a wildcard listener can be reached on, for
// display. Loopback comes first, then Tailscale, then LAN, then anything else.
func Reachable(ifaces []Interface) []Endpoint {
	out := []Endpoint{{loop4, KindLocal}}
	for _, iface := range ifaces {
		if !iface.Up || iface.Loopback {
			continue
		}
		for _, a := range iface.Addrs {
			if k, ok := classify(iface, a); ok && k != KindLocal {
				out = append(out, Endpoint{a, k})
			}
		}
	}
	rank := map[Kind]int{KindLocal: 0, KindTailscale: 1, KindLAN: 2, KindCustom: 3, KindOther: 4}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Kind] < rank[out[j].Kind] })
	return out
}

// SystemInterfaces reads the machine's interfaces. Virtual ones are dropped
// before their addresses are fetched: a Docker host can have dozens of veths,
// and each Addrs call is a separate netlink round trip.
func SystemInterfaces() []Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]Interface, 0, len(ifaces))
	for _, ni := range ifaces {
		iface := Interface{
			Name:     ni.Name,
			Up:       ni.Flags&net.FlagUp != 0,
			Loopback: ni.Flags&net.FlagLoopback != 0,
		}
		if !iface.Up || iface.Loopback {
			continue
		}
		// Tailscale's own adapter matches isVirtual, so it is kept by name.
		lower := strings.ToLower(ni.Name)
		if isVirtual(ni.Name) && !strings.Contains(lower, "tailscale") && !strings.HasPrefix(lower, "utun") {
			continue
		}
		addrs, err := ni.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip, ok := netip.AddrFromSlice(ipnet.IP); ok {
				iface.Addrs = append(iface.Addrs, ip.Unmap())
			}
		}
		out = append(out, iface)
	}
	return out
}

func contains(list []netip.Addr, a netip.Addr) bool {
	for _, x := range list {
		if x == a {
			return true
		}
	}
	return false
}
