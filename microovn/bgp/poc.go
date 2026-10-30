package bgp

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/logger"
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
			case "sb":
				return ovnCmd.SBCtlCluster(ctx, s, args...)
			case "ofctl":
				return shared.RunCommandContext(ctx, filepath.Join(paths.Wrappers(), "ovs-ofctl"), args...)
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

var pocControllerLock sync.Mutex
var pocController *poc.Controller

func controllerPOC(s state.State) *poc.Controller {
	pocControllerLock.Lock()
	defer pocControllerLock.Unlock()
	if pocController == nil {
		pocController = &poc.Controller{Runtime: pocRuntime(s), Report: func(err error) { logger.Warnf("BGP PoC reconciliation failed: %v", err) }}
	}
	return pocController
}

// EnablePOC configures native redirect attachments to externally owned routers.
func EnablePOC(ctx context.Context, s state.State) error { return controllerPOC(s).Enable(ctx) }

// DisablePOC removes only the resources journaled by EnablePOC.
func DisablePOC(ctx context.Context, s state.State) error { return controllerPOC(s).Disable(ctx) }
