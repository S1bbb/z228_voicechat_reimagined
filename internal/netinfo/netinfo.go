// Package netinfo — локальные адреса, VPN-адаптеры (Radmin/Hamachi/ZeroTier/Tailscale) и внешний IP.
package netinfo

import (
	"errors"
	"net"
	"strings"
	"time"

	"github.com/pion/stun/v3"
)

type Addr struct {
	IP    net.IP
	Iface string
	Kind  string // "lan" | "radmin" | "hamachi" | "zerotier" | "tailscale" | "other"
}

var (
	radminNet    = mustCIDR("26.0.0.0/8")
	hamachiNet   = mustCIDR("25.0.0.0/8")
	tailscaleNet = mustCIDR("100.64.0.0/10")
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// LocalIPv4 — все IPv4-адреса поднятых интерфейсов (без loopback) с угаданным типом сети.
func LocalIPv4() []Addr {
	var out []Addr
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, Addr{IP: ip, Iface: iface.Name, Kind: classify(ip, iface.Name)})
		}
	}
	return out
}

func classify(ip net.IP, iface string) string {
	name := strings.ToLower(iface)
	switch {
	case strings.Contains(name, "radmin") || radminNet.Contains(ip):
		return "radmin"
	case strings.Contains(name, "hamachi") || hamachiNet.Contains(ip):
		return "hamachi"
	case strings.Contains(name, "zerotier") || strings.HasPrefix(name, "zt"):
		return "zerotier"
	case strings.Contains(name, "tailscale") || tailscaleNet.Contains(ip):
		return "tailscale"
	case ip.IsPrivate():
		return "lan"
	default:
		return "other"
	}
}

// ByKind — адреса указанного типа.
func ByKind(addrs []Addr, kind string) []Addr {
	var out []Addr
	for _, a := range addrs {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// PublicIP узнаёт внешний IPv4 через STUN-сервер (host:port).
func PublicIP(server string, timeout time.Duration) (net.IP, error) {
	conn, err := net.DialTimeout("udp4", server, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	req := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	if _, err := conn.Write(req.Raw); err != nil {
		return nil, err
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	res := &stun.Message{Raw: buf[:n]}
	if err := res.Decode(); err != nil {
		return nil, err
	}
	var xor stun.XORMappedAddress
	if err := xor.GetFrom(res); err != nil {
		var mapped stun.MappedAddress
		if err2 := mapped.GetFrom(res); err2 != nil {
			return nil, errors.Join(err, err2)
		}
		return mapped.IP, nil
	}
	return xor.IP, nil
}
