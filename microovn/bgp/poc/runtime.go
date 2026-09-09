package poc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Run executes a command without a shell. Command names are nb, ovs, ip, bird,
// birdc, start and stop; callers bind them to MicroOVN's existing wrappers.
type Run func(context.Context, string, ...string) (string, error)

// Runtime applies an explicit snapshot. Re-run enable after desired changes;
// this PoC deliberately does not claim to provide a background reconciler.
type Runtime struct {
	Run        Run
	Dir        string
	BirdConfig string
	Chassis    string
}

type attachment struct {
	Uplink     Uplink `json:"uplink"`
	MAC        string `json:"mac"`
	MappingKey string `json:"mapping_key"`
	OldMapping string `json:"old_mapping"`
}

type journal struct {
	Config      Config       `json:"config"`
	Attachments []attachment `json:"attachments"`
	Bird        []byte       `json:"bird"`
}

func value(s string) string {
	s = strings.TrimSpace(s)
	if v, err := strconv.Unquote(s); err == nil {
		return v
	}
	if s == "[]" {
		return ""
	}
	return s
}

func (r Runtime) get(ctx context.Context, cmd, table, row, column string) (string, error) {
	out, err := r.Run(ctx, cmd, "--if-exists", "get", table, row, column)
	if err != nil && strings.Contains(err.Error(), "no key") {
		return "", nil
	}
	return value(out), err
}

func (r Runtime) path() string { return filepath.Join(r.Dir, "bgp-poc.json") }

func (r Runtime) load() (journal, error) {
	var j journal
	data, err := os.ReadFile(r.path())
	if err != nil {
		return j, err
	}
	err = json.Unmarshal(data, &j)
	if err == nil {
		err = j.Config.Validate()
	}
	return j, err
}

func (r Runtime) save(j journal) error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return atomicFile(r.path(), data, 0600)
}

func atomicFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".bgp-poc-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

// Enable validates the whole configuration and records cleanup information
// before changing any resources. Existing journals permit retry after failure.
func (r Runtime) Enable(ctx context.Context) error {
	raw, err := r.get(ctx, "ovs", "Open_vSwitch", ".", "external_ids:"+ExternalID)
	if err != nil {
		return err
	}
	c, err := Parse([]byte(raw))
	if err != nil {
		return fmt.Errorf("invalid %s: %w", ExternalID, err)
	}
	if !objectName.MatchString(r.Chassis) {
		return fmt.Errorf("invalid chassis identity")
	}
	j, err := r.load()
	if err == nil {
		old, _ := json.Marshal(j.Config)
		desired, _ := json.Marshal(c)
		if string(old) != string(desired) {
			return fmt.Errorf("disable bgp-poc before changing its configuration")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		j, err = r.preflight(ctx, c)
		if err != nil {
			return err
		}
		if err = r.save(j); err != nil {
			return err
		}
	}
	// Validate BIRD syntax before changing topology or replacing the running file.
	candidate := filepath.Join(r.Dir, "bgp-poc-bird.conf")
	if err = atomicFile(candidate, []byte(c.Bird()), 0600); err != nil {
		return err
	}
	if _, err = r.Run(ctx, "bird", "-p", "-c", candidate); err != nil {
		return err
	}
	for _, a := range j.Attachments {
		if err = r.attach(ctx, c, a); err != nil {
			return fmt.Errorf("attach %s: %w (disable bgp-poc cleans up)", a.Uplink.Bridge, err)
		}
	}
	if err = atomicFile(r.BirdConfig, []byte(c.Bird()), 0644); err != nil {
		return err
	}
	if _, err = r.Run(ctx, "start"); err != nil {
		return err
	}
	// snapctl start can return before the BIRD control socket is ready.
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err = r.Run(ctx, "birdc", "configure"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return err
		case <-ticker.C:
		}
	}
}

func (r Runtime) preflight(ctx context.Context, c Config) (journal, error) {
	j := journal{Config: c}
	var err error
	j.Bird, err = os.ReadFile(r.BirdConfig)
	if err != nil {
		return j, err
	}
	mappings, err := r.get(ctx, "ovs", "Open_vSwitch", ".", "external_ids:dynamic-routing-port-mapping")
	if err != nil {
		return j, err
	}
	keys := map[string]bool{}
	for _, u := range c.Uplinks {
		if _, err = r.Run(ctx, "ovs", "br-exists", u.Bridge); err != nil {
			return j, err
		}
		chassis, e := r.get(ctx, "nb", "Logical_Router", u.Router, "options:chassis")
		if e != nil {
			return j, e
		}
		if chassis != r.Chassis {
			return j, fmt.Errorf("router %s is not pinned to local chassis", u.Router)
		}
		vrf, e := r.get(ctx, "nb", "Logical_Router", u.Router, "options:dynamic-routing-vrf-id")
		if e != nil {
			return j, e
		}
		if vrf != strconv.FormatUint(uint64(c.LearnTable), 10) {
			return j, fmt.Errorf("router VRF differs from learn_table")
		}
		ports, e := r.Run(ctx, "nb", "lrp-list", u.Router)
		if e != nil {
			return j, e
		}
		if !strings.Contains(ports, "("+u.RouterPort+")") {
			return j, fmt.Errorf("router port does not belong to router")
		}
		routerLSP, e := r.Run(ctx, "nb", "--bare", "--columns=name", "find", "Logical_Switch_Port", "type=router", "options:router-port="+u.RouterPort)
		if e != nil {
			return j, e
		}
		lsPorts, e := r.Run(ctx, "nb", "lsp-list", u.Switch)
		if e != nil {
			return j, e
		}
		if value(routerLSP) == "" || !strings.Contains(lsPorts, "("+value(routerLSP)+")") {
			return j, fmt.Errorf("router port is not connected to supplied switch")
		}
		for _, key := range []string{"routing-protocol-redirect", "routing-protocols"} {
			old, e := r.get(ctx, "nb", "Logical_Router_Port", u.RouterPort, "options:"+key)
			if e != nil {
				return j, e
			}
			if old != "" {
				return j, fmt.Errorf("router port already has %s", key)
			}
		}
		owner, e := r.get(ctx, "nb", "Logical_Router_Port", u.RouterPort, "external_ids:"+ExternalID)
		if e != nil {
			return j, e
		}
		if owner != "" {
			return j, fmt.Errorf("router port is already owned by %s", owner)
		}
		mac, e := r.get(ctx, "nb", "Logical_Router_Port", u.RouterPort, "mac")
		if e != nil {
			return j, e
		}
		if parsed, e := net.ParseMAC(mac); e != nil || len(parsed) != 6 {
			return j, fmt.Errorf("invalid router port MAC")
		}
		key, e := r.get(ctx, "nb", "Logical_Router_Port", u.RouterPort, "options:dynamic-routing-port-name")
		if e != nil {
			return j, e
		}
		if !objectName.MatchString(key) || keys[key] {
			return j, fmt.Errorf("missing or duplicate dynamic-routing-port-name")
		}
		keys[key] = true
		a := attachment{Uplink: u, MAC: mac, MappingKey: key}
		for _, mapping := range strings.Split(mappings, ",") {
			k, v, ok := strings.Cut(mapping, "=")
			if ok && k == key {
				a.OldMapping = v
			}
		}
		for _, iface := range func() []string { a, b := u.Interfaces(); return []string{a, b} }() {
			if _, e = r.Run(ctx, "ip", "link", "show", "dev", iface); e == nil {
				return j, fmt.Errorf("interface %s already exists", iface)
			}
		}
		name, e := r.get(ctx, "nb", "Logical_Switch_Port", u.PortName(r.Chassis), "name")
		if e != nil {
			return j, e
		}
		if name != "" {
			return j, fmt.Errorf("attachment port already exists")
		}
		j.Attachments = append(j.Attachments, a)
	}
	return j, nil
}

func (r Runtime) mapping(ctx context.Context, key, value string) error {
	old, err := r.get(ctx, "ovs", "Open_vSwitch", ".", "external_ids:dynamic-routing-port-mapping")
	if err != nil {
		return err
	}
	var mappings []string
	for _, m := range strings.Split(old, ",") {
		k, _, ok := strings.Cut(m, "=")
		if ok && k != key {
			mappings = append(mappings, m)
		}
	}
	if value != "" {
		mappings = append(mappings, key+"="+value)
	}
	if len(mappings) == 0 {
		_, err = r.Run(ctx, "ovs", "remove", "Open_vSwitch", ".", "external_ids", "dynamic-routing-port-mapping")
		return err
	}
	_, err = r.Run(ctx, "ovs", "set", "Open_vSwitch", ".", "external_ids:dynamic-routing-port-mapping="+strings.Join(mappings, ","))
	return err
}

func (r Runtime) attach(ctx context.Context, c Config, a attachment) error {
	u := a.Uplink
	host, ovs := u.Interfaces()
	lsp := u.PortName(r.Chassis)
	vrf := fmt.Sprintf("ovnvrf%d", c.LearnTable)
	// OVN owns the VRF. Wait for it instead of creating a competing Linux object.
	if _, err := r.Run(ctx, "ip", "link", "show", "dev", vrf); err != nil {
		return fmt.Errorf("OVN VRF not ready: %w", err)
	}
	if _, err := r.Run(ctx, "ip", "link", "show", "dev", host); err != nil {
		if _, err = r.Run(ctx, "ip", "link", "add", host, "type", "veth", "peer", "name", ovs); err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"link", "set", "dev", host, "address", a.MAC}, {"link", "set", "dev", host, "master", vrf}, {"link", "set", "dev", host, "up"}, {"link", "set", "dev", ovs, "up"}} {
		if _, err := r.Run(ctx, "ip", args...); err != nil {
			return err
		}
	}
	brInt, err := r.get(ctx, "ovs", "Open_vSwitch", ".", "external_ids:ovn-bridge")
	if err != nil {
		return err
	}
	if brInt == "" {
		brInt = "br-int"
	}
	if _, err = r.Run(ctx, "ovs", "--may-exist", "add-port", brInt, ovs, "--", "set", "Interface", ovs, "external_ids:iface-id="+lsp, "external_ids:"+ExternalID+"="+r.Chassis); err != nil {
		return err
	}
	if err = r.mapping(ctx, a.MappingKey, host); err != nil {
		return err
	}
	_, err = r.Run(ctx, "nb", "--may-exist", "lsp-add", u.Switch, lsp, "--", "lsp-set-addresses", lsp, "unknown", "--", "set", "Logical_Switch_Port", lsp, "external_ids:"+ExternalID+"="+r.Chassis, "--", "set", "Logical_Router_Port", u.RouterPort, "external_ids:"+ExternalID+"="+r.Chassis, "options:routing-protocols=BGP,BFD", "options:routing-protocol-redirect="+lsp)
	return err
}

// Disable removes only journaled attachments, preserving externally owned routers.
func (r Runtime) Disable(ctx context.Context) error {
	j, err := r.load()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = r.Run(ctx, "stop"); err != nil {
		return err
	}
	for _, a := range j.Attachments {
		u := a.Uplink
		host, ovs := u.Interfaces()
		lsp := u.PortName(r.Chassis)
		owner, e := r.get(ctx, "nb", "Logical_Router_Port", u.RouterPort, "external_ids:"+ExternalID)
		if e != nil {
			return e
		}
		if owner != "" && owner != r.Chassis {
			return fmt.Errorf("router port ownership changed")
		}
		if owner == r.Chassis {
			redirect, e := r.get(ctx, "nb", "Logical_Router_Port", u.RouterPort, "options:routing-protocol-redirect")
			if e != nil {
				return e
			}
			if redirect != "" && redirect != lsp {
				return fmt.Errorf("redirect ownership changed")
			}
			if _, err = r.Run(ctx, "nb", "--if-exists", "remove", "Logical_Router_Port", u.RouterPort, "options", "routing-protocol-redirect", "routing-protocols", "--", "--if-exists", "remove", "Logical_Router_Port", u.RouterPort, "external_ids", ExternalID); err != nil {
				return err
			}
		}
		owner, e = r.get(ctx, "nb", "Logical_Switch_Port", lsp, "external_ids:"+ExternalID)
		if e != nil {
			return e
		}
		if owner != "" && owner != r.Chassis {
			return fmt.Errorf("attachment ownership changed")
		}
		if owner == r.Chassis {
			if _, err = r.Run(ctx, "nb", "--if-exists", "lsp-del", lsp); err != nil {
				return err
			}
		}
		if _, err = r.Run(ctx, "ovs", "--if-exists", "del-port", ovs); err != nil {
			return err
		}
		if _, e = r.Run(ctx, "ip", "link", "show", "dev", host); e == nil {
			if _, err = r.Run(ctx, "ip", "link", "delete", host); err != nil {
				return err
			}
		}
		if err = r.mapping(ctx, a.MappingKey, a.OldMapping); err != nil {
			return err
		}
	}
	if err = atomicFile(r.BirdConfig, j.Bird, 0644); err != nil {
		return err
	}
	return os.Remove(r.path())
}
