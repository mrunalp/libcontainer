// +build linux

package namespaces

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"

	"github.com/docker/libcontainer"
	"github.com/docker/libcontainer/label"
	"github.com/dotcloud/docker/pkg/system"
	"github.com/docker/libcontainer/apparmor"
	_ "github.com/docker/libcontainer/console"
	"github.com/docker/libcontainer/mount"
	"github.com/docker/libcontainer/network"
	_ "github.com/docker/libcontainer/security/capabilities"
	"github.com/docker/libcontainer/security/restrict"
	"github.com/docker/libcontainer/utils"
	_ "github.com/dotcloud/docker/pkg/user"
)

// ExecIn uses an existing pid and joins the pid's namespaces with the new command.
func ExecIn(container *libcontainer.Config, state *libcontainer.State, args []string) error {
	// TODO(vmarmol): If this gets too long, send it over a pipe to the child.
	// Marshall the container into JSON since it won't be available in the namespace.
	containerJson, err := json.Marshal(container)
	if err != nil {
		return err
	}

	// Enter the namespace and then finish setup
	finalArgs := []string{os.Args[0], "nsenter", "--nspid", strconv.Itoa(state.InitPid), "--containerjson", string(containerJson), "--"}
	finalArgs = append(finalArgs, args...)
	log.Println(os.Getwd())
	log.Println(os.Environ())
	if err := system.Execv(finalArgs[0], finalArgs[0:], os.Environ()); err != nil {
		return err
	}
	panic("unreachable")
}

// NsEnter is run after entering the namespace.
func NsEnter(container *libcontainer.Config, nspid int, args []string, setup bool, uncleanRootfs string, consolePath string, state *libcontainer.State) error {
	// clear the current processes env and replace it with the environment
	// defined on the container

	if setup {
		log.Println("Setup Mode")
		rootfs, err := utils.ResolveRootfs(uncleanRootfs)
		if err != nil {
			return err
		}

		// clear the current processes env and replace it with the environment
		// defined on the container
		if err := LoadContainerEnvironment(container); err != nil {
			return err
		}

		var networkState network.NetworkState
		networkState.VethHost = state.NetworkState.VethHost
		networkState.VethChild = state.NetworkState.VethChild
		networkState.NsPath = state.NetworkState.NsPath

		if err := setupNetwork(container, &networkState); err != nil {
			return fmt.Errorf("setup networking %s", err)
		}
		if err := setupRoute(container); err != nil {
			return fmt.Errorf("setup route %s", err)
		}

		label.Init()

		if err := mount.InitializeMountNamespace(rootfs,
			consolePath,
			(*mount.MountConfig)(container.MountConfig)); err != nil {
			return fmt.Errorf("setup mount namespace %s", err)
		}

		if container.Hostname != "" {
			if err := system.Sethostname(container.Hostname); err != nil {
				return fmt.Errorf("sethostname %s", err)
			}
		}

		runtime.LockOSThread()

		if err := apparmor.ApplyProfile(container.AppArmorProfile); err != nil {
			return fmt.Errorf("set apparmor profile %s: %s", container.AppArmorProfile, err)
		}

		if err := label.SetProcessLabel(container.ProcessLabel); err != nil {
			return fmt.Errorf("set process label %s", err)
		}

		if container.RestrictSys {
			if err := restrict.Restrict("proc/sys", "proc/sysrq-trigger", "proc/irq", "proc/bus", "sys"); err != nil {
				return err
			}
		}
	} else {

	if err := LoadContainerEnvironment(container); err != nil {
		return err
	}
	if err := FinalizeNamespace(container); err != nil {
		return err
	}

	if container.ProcessLabel != "" {
		if err := label.SetProcessLabel(container.ProcessLabel); err != nil {
			return err
		}
	}
	}

	if err := system.Execv(args[0], args[0:], container.Env); err != nil {
		return err
	}
	panic("unreachable")
}
