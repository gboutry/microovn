package poc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func neutronConfig() Config {
	c := valid()
	c.Neutron = &NeutronConfig{Bridge: "br-interconnect", ProviderNetwork: "physnet1", ProviderSwitch: "provider", Switch: "interconnect", RouterPort: "interconnect-router-port"}
	return c
}

func TestNeutronConfigValidation(t *testing.T) {
	c := neutronConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Neutron.ProviderSwitch = c.Neutron.Switch },
		func(c *Config) { c.Neutron.Bridge = c.Uplinks[0].Bridge },
		func(c *Config) { c.Neutron.RouterPort = "x\";bad" },
	} {
		c := neutronConfig()
		change(&c)
		if c.Validate() == nil {
			t.Fatal("accepted invalid compatibility configuration")
		}
	}
}

func TestMappingPreservesColonEntriesAndRestoresTarget(t *testing.T) {
	var written string
	r := Runtime{Run: func(_ context.Context, cmd string, args ...string) (string, error) {
		if args[0] == "--if-exists" {
			return "other:eth0,br-bgp1:old-device,third=eth2", nil
		}
		written = strings.Join(args, " ")
		return "", nil
	}}
	if err := r.mapping(context.Background(), "br-bgp1", "veth-bgp"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(written, "other:eth0,third=eth2,br-bgp1=veth-bgp") {
		t.Fatal(written)
	}
}

func interconnectRuntime(t *testing.T, dump string) (Runtime, *string) {
	t.Helper()
	var installed string
	r := Runtime{Dir: t.TempDir(), Run: func(_ context.Context, cmd string, args ...string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case cmd == "ofctl" && args[0] == "dump-flows":
			return dump, nil
		case cmd == "ofctl":
			data, err := os.ReadFile(args[len(args)-1])
			installed = string(data)
			return "", err
		case strings.Contains(call, "get Logical_Router_Port"):
			return "02:00:00:00:00:01", nil
		case strings.Contains(call, "find Logical_Switch_Port"):
			return "interconnect-r", nil
		case call == "lsp-list interconnect":
			return "uuid (interconnect-r)\nuuid (bgp-localnet)", nil
		case call == "lsp-list provider":
			return "uuid (provider-localnet)", nil
		case cmd == "ovs" && args[0] == "list-ports":
			return "patch-a\npatch-b", nil
		case strings.HasSuffix(call, " type"):
			return "patch", nil
		case strings.Contains(call, "get Port patch-a"):
			return "bgp-localnet", nil
		case strings.Contains(call, "get Port patch-b"):
			return "provider-localnet", nil
		case strings.Contains(call, "options:network_name"):
			return "physnet1", nil
		case strings.Contains(call, "patch-a ofport"):
			return "7", nil
		case strings.Contains(call, "patch-b ofport"):
			return "8", nil
		default:
			return "", fmt.Errorf("unexpected %s %s", cmd, call)
		}
	}}
	return r, &installed
}

func TestInterconnectFlowsUseMetadataAndScopedBundle(t *testing.T) {
	r, installed := interconnectRuntime(t, "cookie=0, priority=0 actions=NORMAL\n")
	if err := r.neutronFlows(context.Background(), neutronConfig()); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"delete cookie=" + flowCookie + "/-1\n",
		"priority=10,in_port=8 actions=mod_dl_dst:02:00:00:00:00:01,output:7",
		"priority=10,in_port=7 actions=output:8",
	} {
		if !strings.Contains(*installed, required) {
			t.Fatal(*installed)
		}
	}
	if strings.Contains(*installed, "delete\n") {
		t.Fatal("unscoped deletion")
	}
}

func TestInterconnectRejectsForeignFlows(t *testing.T) {
	r, installed := interconnectRuntime(t, "cookie=0, priority=10,in_port=8 actions=output:7\n")
	if err := r.neutronFlows(context.Background(), neutronConfig()); err == nil {
		t.Fatal("accepted conflicting extension flows")
	}
	if *installed != "" {
		t.Fatal("changed flows before rejecting conflict")
	}
}

func TestInterconnectReconcileKeepsUnchangedFlows(t *testing.T) {
	dump := "cookie=" + flowCookie + ", duration=1s, priority=10,in_port=8 actions=mod_dl_dst:02:00:00:00:00:01,output:7\n" +
		"cookie=" + flowCookie + ", duration=1s, priority=10,in_port=7 actions=output:8\n"
	r, installed := interconnectRuntime(t, dump)
	if err := r.neutronFlows(context.Background(), neutronConfig()); err != nil {
		t.Fatal(err)
	}
	if *installed != "" {
		t.Fatal("replaced unchanged flows and reset their counters")
	}
}

func TestAttachmentRevalidationRejectsChangedMAC(t *testing.T) {
	c := valid()
	a := attachment{Uplink: c.Uplinks[0], MAC: "02:00:00:00:00:01", MappingKey: "br-bgp1"}
	r := Runtime{Chassis: "local", Run: func(_ context.Context, cmd string, args ...string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.HasSuffix(call, "options:chassis"):
			return "local", nil
		case strings.HasSuffix(call, "options:dynamic-routing-vrf-id"):
			return "10", nil
		case strings.HasSuffix(call, " mac"):
			return "02:00:00:00:00:02", nil
		}
		t.Fatalf("unexpected operation: %s %s", cmd, call)
		return "", nil
	}}
	if err := r.validateAttachment(context.Background(), c, a); err == nil {
		t.Fatal("accepted changed identity")
	}
}

func TestAttachmentReattachesToExistingVRF(t *testing.T) {
	c := valid()
	a := attachment{Uplink: c.Uplinks[0], MAC: "02:00:00:00:00:01", MappingKey: "br-bgp1"}
	var calls []string
	r := Runtime{Chassis: "local", Run: func(_ context.Context, cmd string, args ...string) (string, error) {
		calls = append(calls, cmd+" "+strings.Join(args, " "))
		return "", nil
	}}
	if err := r.attach(context.Background(), c, a); err != nil {
		t.Fatal(err)
	}
	host, _ := a.Uplink.Interfaces()
	if !strings.Contains(strings.Join(calls, "\n"), "ip link set dev "+host+" master ovnvrf10") {
		t.Fatal(calls)
	}
}

func TestLocalAttachmentRejectsForeignInterface(t *testing.T) {
	r := Runtime{Chassis: "local", Run: func(_ context.Context, cmd string, args ...string) (string, error) {
		if cmd != "ovs" || args[0] != "--if-exists" {
			t.Fatalf("unexpected mutation: %s %v", cmd, args)
		}
		if args[len(args)-1] == "name" {
			return "attachment", nil
		}
		return "foreign", nil
	}}
	if err := r.validateLocalInterface(context.Background(), "attachment"); err == nil {
		t.Fatal("accepted a foreign interface")
	}
}

func TestControllerRetriesAfterMissingPrerequisiteAndStopsBeforeCleanup(t *testing.T) {
	c := neutronConfig()
	raw, _ := json.Marshal(c)
	seen := make(chan struct{}, 8)
	r := Runtime{Dir: t.TempDir(), BirdConfig: filepath.Join(t.TempDir(), "bird.conf"), Chassis: "local"}
	if err := os.WriteFile(r.BirdConfig, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	r.Run = func(_ context.Context, cmd string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "external_ids:"+ExternalID) {
			return string(raw), nil
		}
		if strings.Contains(strings.Join(args, " "), "ovn-bridge-mappings") {
			return "", nil
		}
		if cmd == "sb" {
			return "", fmt.Errorf("SB unavailable")
		}
		return "", nil
	}
	controller := Controller{Runtime: r, Report: func(err error) { seen <- struct{}{} }}
	if controller.Enable(context.Background()) == nil {
		t.Fatal("missing SB should fail initial apply")
	}
	defer controller.Disable(context.Background())
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not retry")
	}
	if err := controller.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if controller.cancel != nil {
		t.Fatal("worker survived cleanup")
	}
}
