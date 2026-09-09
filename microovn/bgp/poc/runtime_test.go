package poc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreflightRejectsForeignChassisBeforeMutation(t *testing.T) {
	c := valid()
	raw, _ := json.Marshal(c)
	dir := t.TempDir()
	bird := filepath.Join(dir, "bird.conf")
	if err := os.WriteFile(bird, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	r := Runtime{Dir: dir, BirdConfig: bird, Chassis: "local", Run: func(_ context.Context, cmd string, args ...string) (string, error) {
		call := cmd + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch {
		case strings.Contains(call, "external_ids:"+ExternalID):
			return string(raw), nil
		case strings.Contains(call, "options:chassis"):
			return "foreign", nil
		default:
			return "", nil
		}
	}}
	if err := r.Enable(context.Background()); err == nil || !strings.Contains(err.Error(), "local chassis") {
		t.Fatalf("got %v", err)
	}
	for _, call := range calls {
		if !strings.Contains(call, " get ") && !strings.Contains(call, "br-exists") {
			t.Fatalf("unexpected mutation: %s", call)
		}
	}
	if _, err := os.Stat(r.path()); !os.IsNotExist(err) {
		t.Fatal("journal created for invalid request")
	}
}

func TestDisablePreservesExternalTopologyAndUnrelatedMappings(t *testing.T) {
	c := valid()
	dir := t.TempDir()
	a := attachment{Uplink: c.Uplinks[0], MappingKey: "br-bgp1", OldMapping: "old-device"}
	var calls []string
	r := Runtime{Dir: dir, BirdConfig: filepath.Join(dir, "bird.conf"), Chassis: "local"}
	r.Run = func(_ context.Context, cmd string, args ...string) (string, error) {
		call := cmd + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch {
		case strings.Contains(call, " get ") && strings.Contains(call, "external_ids:"+ExternalID):
			return "local", nil
		case strings.Contains(call, " get ") && strings.Contains(call, "options:routing-protocol-redirect"):
			return a.Uplink.PortName("local"), nil
		case strings.Contains(call, " get ") && strings.Contains(call, "dynamic-routing-port-mapping"):
			return "unrelated=keep,br-bgp1=poc-device", nil
		case cmd == "ip" && strings.Contains(call, "show"):
			return "", fmt.Errorf("missing interface")
		default:
			return "", nil
		}
	}
	if err := r.save(journal{Config: c, Attachments: []attachment{a}, Bird: []byte("original")}); err != nil {
		t.Fatal(err)
	}
	if err := r.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(calls, "\n")
	for _, forbidden := range []string{"lr-del", "lrp-del", "ls-del", "ofctl"} {
		if strings.Contains(all, forbidden) {
			t.Fatal(all)
		}
	}
	if !strings.Contains(all, "unrelated=keep,br-bgp1=old-device") {
		t.Fatal(all)
	}
	data, err := os.ReadFile(r.BirdConfig)
	if err != nil || string(data) != "original" {
		t.Fatal(string(data), err)
	}
	if err := r.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveLastMappingUsesMapRemoval(t *testing.T) {
	var calls []string
	r := Runtime{Run: func(_ context.Context, cmd string, args ...string) (string, error) {
		call := cmd + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if strings.Contains(call, " get ") {
			return "br-bgp=owned", nil
		}
		if strings.HasSuffix(call, "=") {
			return "", fmt.Errorf("OVS requires a value")
		}
		return "", nil
	}}
	if err := r.mapping(context.Background(), "br-bgp", ""); err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1] != "ovs remove Open_vSwitch . external_ids dynamic-routing-port-mapping" {
		t.Fatal(calls)
	}
}
