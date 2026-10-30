package poc

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const neutronBridges = "neutron-bgp-bridges"
const flowCookie = "0x6d6963726f706f63"

type neutronJournal struct {
	Mappings map[string]string `json:"mappings"`
	Bridges  string            `json:"bridges"`
}

// OVN accepts both separators. Preserve unrelated entries in their original form.
func splitMapping(s string) (string, string, bool) {
	i := strings.IndexAny(s, ":=")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

func (c Config) bridgeMappings() map[string]string {
	m := map[string]string{c.Neutron.ProviderNetwork: c.Neutron.Bridge}
	for _, u := range c.Uplinks {
		m[u.Bridge] = u.Bridge
	}
	return m
}

func (r Runtime) neutronSnapshot(ctx context.Context, c Config) (journal, error) {
	j := journal{Config: c, Neutron: &neutronJournal{Mappings: map[string]string{}}}
	var err error
	j.Bird, err = os.ReadFile(r.BirdConfig)
	if err != nil {
		return j, err
	}
	current, err := r.get(ctx, "ovs", "Open_vSwitch", ".", "external_ids:ovn-bridge-mappings")
	if err != nil {
		return j, err
	}
	for k, wanted := range c.bridgeMappings() {
		j.Neutron.Mappings[k] = ""
		for _, entry := range strings.Split(current, ",") {
			key, old, ok := splitMapping(entry)
			if ok && key == k {
				if old != wanted {
					return j, fmt.Errorf("bridge mapping %s is already assigned to %s", k, old)
				}
				j.Neutron.Mappings[k] = old
			}
		}
	}
	j.Neutron.Bridges, err = r.get(ctx, "sb", "Chassis_Private", r.Chassis, "external_ids:"+neutronBridges)
	return j, err
}

func (r Runtime) bridgeMapping(ctx context.Context, key, desired string) error {
	current, err := r.get(ctx, "ovs", "Open_vSwitch", ".", "external_ids:ovn-bridge-mappings")
	if err != nil {
		return err
	}
	var entries []string
	for _, entry := range strings.Split(current, ",") {
		k, _, ok := splitMapping(entry)
		if entry != "" && (!ok || k != key) {
			entries = append(entries, entry)
		}
	}
	if desired != "" {
		entries = append(entries, key+":"+desired)
	}
	sort.Strings(entries)
	updated := strings.Join(entries, ",")
	if updated == current {
		return nil
	}
	if updated == "" {
		_, err = r.Run(ctx, "ovs", "remove", "Open_vSwitch", ".", "external_ids", "ovn-bridge-mappings")
	} else {
		_, err = r.Run(ctx, "ovs", "set", "Open_vSwitch", ".", "external_ids:ovn-bridge-mappings="+updated)
	}
	return err
}

func (r Runtime) neutronPublish(ctx context.Context, c Config) error {
	for key, bridge := range c.bridgeMappings() {
		if _, err := r.Run(ctx, "ovs", "br-exists", bridge); err != nil {
			return err
		}
		if err := r.bridgeMapping(ctx, key, bridge); err != nil {
			return err
		}
	}
	var bridges []string
	for _, u := range c.Uplinks {
		bridges = append(bridges, u.Bridge)
	}
	sort.Strings(bridges)
	wanted := strings.Join(bridges, ",")
	old, err := r.get(ctx, "sb", "Chassis_Private", r.Chassis, "external_ids:"+neutronBridges)
	if err != nil {
		return err
	}
	if old != wanted {
		_, err = r.Run(ctx, "sb", "set", "Chassis_Private", r.Chassis, "external_ids:"+neutronBridges+"="+wanted)
	}
	return err
}

// Resolve patch ports through OVN's metadata, not Neutron's port-name convention.
func (r Runtime) interconnect(ctx context.Context, n NeutronConfig) ([]string, error) {
	mac, err := r.get(ctx, "nb", "Logical_Router_Port", n.RouterPort, "mac")
	if err != nil {
		return nil, err
	}
	parsed, err := net.ParseMAC(mac)
	if err != nil || len(parsed) != 6 || parsed[0]&1 != 0 {
		return nil, fmt.Errorf("invalid interconnect router MAC")
	}
	routerLSP, err := r.Run(ctx, "nb", "--bare", "--columns=name", "find", "Logical_Switch_Port", "type=router", "options:router-port="+n.RouterPort)
	if err != nil {
		return nil, err
	}
	bgpPorts, err := r.Run(ctx, "nb", "lsp-list", n.Switch)
	if err != nil {
		return nil, err
	}
	if value(routerLSP) == "" || !strings.Contains(bgpPorts, "("+value(routerLSP)+")") {
		return nil, fmt.Errorf("interconnect router port is not on supplied switch")
	}
	providerPorts, err := r.Run(ctx, "nb", "lsp-list", n.ProviderSwitch)
	if err != nil {
		return nil, err
	}
	ports, err := r.Run(ctx, "ovs", "list-ports", n.Bridge)
	if err != nil {
		return nil, err
	}
	provider, bgp := 0, 0
	for _, port := range strings.Fields(ports) {
		kind, e := r.get(ctx, "ovs", "Interface", port, "type")
		if e != nil {
			return nil, e
		}
		if kind != "patch" {
			return nil, fmt.Errorf("interconnect bridge must contain only its two OVN patch ports")
		}
		lsp, e := r.get(ctx, "ovs", "Port", port, "external_ids:ovn-localnet-port")
		if e != nil {
			return nil, e
		}
		network, e := r.get(ctx, "nb", "Logical_Switch_Port", lsp, "options:network_name")
		if e != nil {
			return nil, e
		}
		if network != n.ProviderNetwork {
			return nil, fmt.Errorf("unexpected interconnect localnet mapping")
		}
		number, e := r.get(ctx, "ovs", "Interface", port, "ofport")
		if e != nil {
			return nil, e
		}
		idx, e := strconv.Atoi(number)
		if e != nil || idx <= 0 {
			return nil, fmt.Errorf("interconnect ofport is not ready")
		}
		switch {
		case strings.Contains(providerPorts, "("+lsp+")"):
			if provider != 0 {
				return nil, fmt.Errorf("duplicate provider patch")
			}
			provider = idx
		case strings.Contains(bgpPorts, "("+lsp+")"):
			if bgp != 0 {
				return nil, fmt.Errorf("duplicate BGP patch")
			}
			bgp = idx
		default:
			return nil, fmt.Errorf("patch does not belong to configured logical switches")
		}
	}
	if provider == 0 || bgp == 0 {
		return nil, fmt.Errorf("interconnect patch ports are not ready")
	}
	return []string{
		fmt.Sprintf("priority=10,in_port=%d actions=mod_dl_dst:%s,output:%d", provider, mac, bgp),
		fmt.Sprintf("priority=10,in_port=%d actions=output:%d", bgp, provider),
	}, nil
}

func (r Runtime) neutronFlows(ctx context.Context, c Config) error {
	wanted, err := r.interconnect(ctx, *c.Neutron)
	if err != nil {
		return err
	}
	dump, err := r.Run(ctx, "ofctl", "dump-flows", c.Neutron.Bridge)
	if err != nil {
		return err
	}
	matched := map[string]bool{}
	owned := 0
	for _, line := range strings.Split(dump, "\n") {
		_, tail, ok := strings.Cut(line, "priority=")
		if !ok {
			continue
		}
		if strings.Contains(line, "cookie="+flowCookie+",") {
			owned++
			matched["priority="+strings.TrimSpace(tail)] = true
		} else if strings.HasPrefix(tail, "10,") {
			return fmt.Errorf("remove the disabled BGP extension's priority-10 interconnect flows before takeover")
		}
	}
	if owned == 2 && matched[wanted[0]] && matched[wanted[1]] {
		return nil
	}
	var lines []string
	lines = append(lines, "delete cookie="+flowCookie+"/-1")
	for _, flow := range wanted {
		lines = append(lines, "add cookie="+flowCookie+","+flow)
	}
	file := filepath.Join(r.Dir, "bgp-poc-interconnect.flows")
	if err = atomicFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		return err
	}
	defer os.Remove(file)
	_, err = r.Run(ctx, "ofctl", "--bundle", "add-flows", c.Neutron.Bridge, file)
	return err
}

func (r Runtime) neutronCleanup(ctx context.Context, j journal) error {
	n := j.Config.Neutron
	if n == nil {
		return fmt.Errorf("invalid compatibility journal")
	}
	if _, err := r.Run(ctx, "ovs", "br-exists", n.Bridge); err == nil {
		if _, err = r.Run(ctx, "ofctl", "del-flows", n.Bridge, "cookie="+flowCookie+"/-1"); err != nil {
			return err
		}
	}
	for key, old := range j.Neutron.Mappings {
		if err := r.bridgeMapping(ctx, key, old); err != nil {
			return err
		}
	}
	var err error
	if j.Neutron.Bridges == "" {
		_, err = r.Run(ctx, "sb", "--if-exists", "remove", "Chassis_Private", r.Chassis, "external_ids", neutronBridges)
	} else {
		_, err = r.Run(ctx, "sb", "set", "Chassis_Private", r.Chassis, "external_ids:"+neutronBridges+"="+j.Neutron.Bridges)
	}
	return err
}

func (r Runtime) validateAttachment(ctx context.Context, c Config, a attachment) error {
	checks := []struct{ table, row, column, wanted string }{
		{"Logical_Router", a.Uplink.Router, "options:chassis", r.Chassis},
		{"Logical_Router", a.Uplink.Router, "options:dynamic-routing-vrf-id", strconv.FormatUint(uint64(c.LearnTable), 10)},
		{"Logical_Router_Port", a.Uplink.RouterPort, "mac", a.MAC},
		{"Logical_Router_Port", a.Uplink.RouterPort, "options:dynamic-routing-port-name", a.MappingKey},
	}
	for _, check := range checks {
		actual, err := r.get(ctx, "nb", check.table, check.row, check.column)
		if err != nil {
			return err
		}
		if actual != check.wanted {
			return fmt.Errorf("attachment identity changed: %s %s", check.row, check.column)
		}
	}
	for _, column := range []string{"options:routing-protocol-redirect", "external_ids:" + ExternalID} {
		actual, err := r.get(ctx, "nb", "Logical_Router_Port", a.Uplink.RouterPort, column)
		if err != nil {
			return err
		}
		wanted := r.Chassis
		if strings.HasPrefix(column, "options:") {
			wanted = a.Uplink.PortName(r.Chassis)
		}
		if actual != "" && actual != wanted {
			return fmt.Errorf("attachment ownership changed")
		}
	}
	owner, err := r.get(ctx, "nb", "Logical_Switch_Port", a.Uplink.PortName(r.Chassis), "external_ids:"+ExternalID)
	if err != nil {
		return err
	}
	if owner != "" && owner != r.Chassis {
		return fmt.Errorf("attachment port ownership changed")
	}
	if owner == "" {
		name, err := r.get(ctx, "nb", "Logical_Switch_Port", a.Uplink.PortName(r.Chassis), "name")
		if err != nil {
			return err
		}
		if name != "" {
			return fmt.Errorf("attachment port has no ownership marker")
		}
	}
	ports, err := r.Run(ctx, "nb", "lrp-list", a.Uplink.Router)
	if err != nil {
		return err
	}
	if !strings.Contains(ports, "("+a.Uplink.RouterPort+")") {
		return fmt.Errorf("attachment router port moved")
	}
	routerLSP, err := r.Run(ctx, "nb", "--bare", "--columns=name", "find", "Logical_Switch_Port", "type=router", "options:router-port="+a.Uplink.RouterPort)
	if err != nil {
		return err
	}
	lsPorts, err := r.Run(ctx, "nb", "lsp-list", a.Uplink.Switch)
	if err != nil {
		return err
	}
	if value(routerLSP) == "" || !strings.Contains(lsPorts, "("+value(routerLSP)+")") {
		return fmt.Errorf("attachment switch relationship changed")
	}
	return nil
}
