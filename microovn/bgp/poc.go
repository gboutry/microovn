package bgp

import (
	"context"
	"os"
	"path/filepath"

	"github.com/canonical/lxd/shared"
	"github.com/canonical/microcluster/v3/state"
	"github.com/canonical/microovn/microovn/bgp/poc"
	ovnCmd "github.com/canonical/microovn/microovn/ovn/cmd"
	"github.com/canonical/microovn/microovn/ovn/paths"
	"github.com/canonical/microovn/microovn/snap"
)

func pocRuntime(s state.State) poc.Runtime {
	return poc.Runtime{Dir: paths.BirdConfigDir(), BirdConfig: paths.BirdConfigFile(), Chassis: s.Name(),
		Run: func(ctx context.Context, command string, args ...string) (string, error) {
			switch command {
			case "nb":
				return ovnCmd.NBCtlCluster(ctx, s, args...)
			case "ovs":
				return ovnCmd.VSCtl(ctx, s, args...)
			case "start":
				return "", snap.Start(ctx, BirdService, true)
			case "stop":
				return "", snap.Stop(ctx, BirdService, true)
			case "bird":
				return shared.RunCommandContext(ctx, filepath.Join(os.Getenv("SNAP"), "usr/sbin/bird"), args...)
			case "birdc":
				return shared.RunCommandContext(ctx, filepath.Join(paths.Wrappers(), command), args...)
			default:
				return shared.RunCommandContext(ctx, command, args...)
			}
		}}
}

// EnablePOC configures native redirect attachments to externally owned routers.
func EnablePOC(ctx context.Context, s state.State) error { return pocRuntime(s).Enable(ctx) }

// DisablePOC removes only the resources journaled by EnablePOC.
func DisablePOC(ctx context.Context, s state.State) error { return pocRuntime(s).Disable(ctx) }
