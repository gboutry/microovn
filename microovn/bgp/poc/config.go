// Package poc implements an experimental attachment to externally owned OVN routers.
package poc

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
)

// ExternalID holds a complete, versioned desired configuration on Open_vSwitch.
const ExternalID = "microovn-bgp-poc"

// Config is supplied by the external topology owner, without CMS-specific keys.
type Config struct {
	Version        int      `json:"version"`
	RouterID       string   `json:"router_id"`
	LocalASN       uint32   `json:"local_asn"`
	AdvertiseTable uint32   `json:"advertise_table"`
	LearnTable     uint32   `json:"learn_table"`
	Uplinks        []Uplink `json:"uplinks"`
}

// Uplink attaches an existing router port to a host routing daemon.
type Uplink struct {
	Bridge     string `json:"bridge"`
	Router     string `json:"router"`
	RouterPort string `json:"router_port"`
	Switch     string `json:"switch"`
	Peers      []Peer `json:"peers"`
}

// Peer identifies an IPv6 link-local eBGP session on an uplink.
// The first PoC supports IPv4 NLRI with IPv6 extended next hops.
type Peer struct {
	Address string `json:"address"`
	ASN     uint32 `json:"asn"`
}

var objectName = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.:-]*$`)

// Parse rejects unsupported fields rather than silently accepting ineffective config.
func Parse(data []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("expected one JSON object")
	}
	return c, c.Validate()
}

// Validate checks the supported PoC boundary before any mutation.
func (c Config) Validate() error {
	id, err := netip.ParseAddr(c.RouterID)
	if c.Version != 1 || err != nil || !id.Is4() || id.IsUnspecified() || c.LocalASN == 0 {
		return fmt.Errorf("require version 1, an IPv4 router_id and nonzero local_asn")
	}
	if c.AdvertiseTable == c.LearnTable || c.AdvertiseTable == 0 || c.LearnTable == 0 ||
		(c.AdvertiseTable >= 253 && c.AdvertiseTable <= 255) || (c.LearnTable >= 253 && c.LearnTable <= 255) {
		return fmt.Errorf("require distinct, non-reserved advertise_table and learn_table")
	}
	if len(c.Uplinks) == 0 {
		return fmt.Errorf("at least one uplink is required")
	}
	bridges, ports := map[string]bool{}, map[string]bool{}
	for _, u := range c.Uplinks {
		for _, name := range []string{u.Bridge, u.Router, u.RouterPort, u.Switch} {
			if !objectName.MatchString(name) || len(name) > 240 {
				return fmt.Errorf("invalid object name %q", name)
			}
		}
		if len(u.Bridge) > 15 || bridges[u.Bridge] || ports[u.RouterPort] {
			return fmt.Errorf("duplicate uplink or invalid bridge %q", u.Bridge)
		}
		bridges[u.Bridge], ports[u.RouterPort] = true, true
		if len(u.Peers) == 0 {
			return fmt.Errorf("uplink %s needs peers", u.Bridge)
		}
		peers := map[string]bool{}
		for _, p := range u.Peers {
			ip, err := netip.ParseAddr(p.Address)
			if err != nil || !ip.Is6() || !ip.IsLinkLocalUnicast() || ip.Zone() != "" || p.ASN == 0 || p.ASN == c.LocalASN || peers[ip.String()] {
				return fmt.Errorf("invalid or duplicate link-local eBGP peer on %s", u.Bridge)
			}
			peers[ip.String()] = true
		}
	}
	return nil
}

// Interfaces returns deterministic Linux names independent of long logical names.
func (u Uplink) Interfaces() (string, string) {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(u.Bridge)))[:8]
	return "mp" + hash + "b", "mp" + hash + "o"
}

// PortName identifies the attachment without colliding with the router port.
func (u Uplink) PortName(chassis string) string {
	_, ovs := u.Interfaces()
	return "microovn-bgp-poc-" + chassis + "-" + ovs
}

// Bird renders separate advertisement and learned-route channels. Only host
// routes learned from the advertisement kernel protocol can be exported to peers.
func (c Config) Bird() string {
	var b strings.Builder
	fmt.Fprintf(&b, "router id %s;\nprotocol device {}\n", c.RouterID)
	fmt.Fprintf(&b, `protocol kernel advertisements {
 kernel table %d;
 learn;
 ipv4 { import filter { if net.len = 32 then accept; reject; }; export none; };
}
ipv4 table learned;
protocol pipe to_ovn {
 table master4;
 peer table learned;
 import none;
 export filter { if source = RTS_BGP && net = 0.0.0.0/0 then accept; reject; };
}
protocol kernel egress {
 kernel table %d;
 ipv4 { table learned; import none; export all; };
}
`, c.AdvertiseTable, c.LearnTable)
	for i, u := range c.Uplinks {
		iface, _ := u.Interfaces()
		for j, p := range u.Peers {
			fmt.Fprintf(&b, `protocol bgp uplink_%d_peer_%d {
 vrf "ovnvrf%d";
 interface "%s";
 local as %d;
 neighbor %s as %d;
 ipv4 {
  extended next hop on;
  require extended next hop on;
  next hop self;
  import filter { if net = 0.0.0.0/0 then accept; reject; };
  export filter { if proto = "advertisements" && net.len = 32 then accept; reject; };
 };
}
`, i, j, c.LearnTable, iface, c.LocalASN, p.Address, p.ASN)
		}
	}
	return b.String()
}
