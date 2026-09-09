package poc

import (
	"strings"
	"testing"
)

func valid() Config {
	return Config{Version: 1, RouterID: "192.0.2.1", LocalASN: 65101, AdvertiseTable: 42, LearnTable: 10,
		Uplinks: []Uplink{{Bridge: "br-bgp1", Router: "external-router", RouterPort: "external-port", Switch: "external-switch", Peers: []Peer{{Address: "fe80::1", ASN: 65002}}}}}
}

func TestMultipleToRs(t *testing.T) {
	c := valid()
	c.Uplinks[0].Peers = append(c.Uplinks[0].Peers, Peer{Address: "fe80::2", ASN: 65003})
	c.Uplinks = append(c.Uplinks, Uplink{Bridge: "br-bgp2", Router: "external-router", RouterPort: "other-port", Switch: "other-switch", Peers: []Peer{{Address: "fe80::1", ASN: 65004}}})
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	bird := c.Bird()
	if strings.Count(bird, "protocol bgp ") != 3 {
		t.Fatal(bird)
	}
	a, _ := c.Uplinks[0].Interfaces()
	b, _ := c.Uplinks[1].Interfaces()
	if a == b || !strings.Contains(bird, `interface "`+a+`"`) || !strings.Contains(bird, `interface "`+b+`"`) {
		t.Fatal(bird)
	}
	if !strings.Contains(bird, "kernel table 42") || !strings.Contains(bird, "kernel table 10") || !strings.Contains(bird, `proto = "advertisements"`) {
		t.Fatal(bird)
	}
}

func TestRejectUnsupportedAndUnsafeConfig(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.LearnTable = 42 },
		func(c *Config) { c.AdvertiseTable = 254 },
		func(c *Config) { c.Uplinks[0].Bridge = "x\";bad" },
		func(c *Config) { c.Uplinks[0].Peers[0].Address = "192.0.2.2" },
		func(c *Config) { c.Uplinks[0].Peers[0].Address = "fe80::1%eth0" },
		func(c *Config) { c.Uplinks = append(c.Uplinks, c.Uplinks[0]) },
	} {
		c := valid()
		change(&c)
		if c.Validate() == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	if _, err := Parse([]byte(`{"unknown":true}`)); err == nil {
		t.Fatal("accepted unknown field")
	}
}
