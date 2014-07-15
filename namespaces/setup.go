package namespaces

import (
	"fmt"
	"os"
	"runtime"
	"syscall"

	"github.com/docker/libcontainer"
	"github.com/docker/libcontainer/apparmor"
	_ "github.com/docker/libcontainer/console"
	"github.com/docker/libcontainer/label"
	"github.com/docker/libcontainer/mount"
	_ "github.com/docker/libcontainer/security/capabilities"
	"github.com/docker/libcontainer/security/restrict"
	"github.com/docker/libcontainer/utils"
	"github.com/dotcloud/docker/pkg/system"
	_ "github.com/dotcloud/docker/pkg/user"
)

const (
        SYS_SETNS  = 308 // look here for different arch http://git.kernel.org/cgit/linux/kernel/git/torvalds/linux.git/commit/?id=7b21fddd087678a70ad64afc0f632e0f1071b092
)

func setns(fd uintptr, flags uintptr) error {
	_, _, err := syscall.RawSyscall(SYS_SETNS, fd, flags, 0)
	if err != 0 {
		return err
	}
	return nil
}

func JoinNamespaces(pid int) error {
	namespaces := []string{"ipc", "mnt", "net", "uts"}
	for ns := range namespaces {
		fPath := fmt.Sprintf("/proc/%v/%v", pid, ns)
		file, err := os.Open(fPath)
		if err != nil {
			return err
		}
		defer file.Close()

		if err := setns(file.Fd(), 0); err != nil {
			return err
		}
	}
	return nil
}

func SetupContainer(container *libcontainer.Config, uncleanRootfs, consolePath string, syncPipe *SyncPipe, initPid int) (err error) {
	defer func() {
		if err != nil {
			syncPipe.ReportChildError(fmt.Errorf("SetupContainer: %v", err))
		}
	}()

	// Join all namespaces except pid/user
	if err := JoinNamespaces(initPid); err != nil {
		return err
	}

	rootfs, err := utils.ResolveRootfs(uncleanRootfs)
	if err != nil {
		return err
	}

	// clear the current processes env and replace it with the environment
	// defined on the container
	if err := LoadContainerEnvironment(container); err != nil {
		return err
	}

	// We always read this as it is a way to sync with the parent as well
	networkState, err := syncPipe.ReadFromParent()
	if err != nil {
		return fmt.Errorf("ReadFromParent: %s", err)
	}

	if err := setupNetwork(container, networkState); err != nil {
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

	return nil
}
