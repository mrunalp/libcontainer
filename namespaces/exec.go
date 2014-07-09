// +build linux

package namespaces

import (
	"fmt"
	"log"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/docker/libcontainer"
	"github.com/docker/libcontainer/cgroups"
	"github.com/docker/libcontainer/cgroups/fs"
	"github.com/docker/libcontainer/cgroups/systemd"
	"github.com/docker/libcontainer/network"
	"github.com/dotcloud/docker/pkg/system"
)

// Write UID/GID mappings for a process.
func writeUserMappings(pid int, uidMappings, gidMappings []libcontainer.IdMap) error {
	if len(uidMappings) > 5 || len(gidMappings) > 5 {
		return fmt.Errorf("Only 5 uid/gid mappings are supported by the kernel")
	}

	uidMapStr := make([]string, len(uidMappings))
	for i, um := range uidMappings {
		uidMapStr[i] = fmt.Sprintf("%v %v %v", um.ContainerId, um.HostId, um.Size)
	}

	gidMapStr := make([]string, len(gidMappings))
	for i, gm := range gidMappings {
		gidMapStr[i] = fmt.Sprintf("%v %v %v", gm.ContainerId, gm.HostId, gm.Size)
	}

	uidMap := []byte(strings.Join(uidMapStr, "\n"))
	gidMap := []byte(strings.Join(gidMapStr, "\n"))

	uidMappingsFile := fmt.Sprintf("/proc/%v/uid_map", pid)
	gidMappingsFile := fmt.Sprintf("/proc/%v/gid_map", pid)

	if err := ioutil.WriteFile(uidMappingsFile, uidMap, 0644); err != nil {
		return err
	}
	if err := ioutil.WriteFile(gidMappingsFile, gidMap, 0644); err != nil {
		return err
	}

	return nil
}

// TODO(vishh): This is part of the libcontainer API and it does much more than just namespaces related work.
// Move this to libcontainer package.
// Exec performs setup outside of a namespace so that a container can be
// executed.  Exec is a high level function for working with container namespaces.
func Exec(container *libcontainer.Config, term Terminal, rootfs, dataPath string, args []string, createCommand CreateCommand, setupCommand CreateCommand, startCallback func()) (int, error) {
	var (
		master  *os.File
		console string
		err     error
	)

	// create a pipe so that we can syncronize with the namespaced process and
	// pass the veth name to the child
	syncPipe, err := NewSyncPipe()
	if err != nil {
		return -1, err
	}
	defer syncPipe.Close()

	if container.Tty {
		master, console, err = system.CreateMasterAndConsole()
		if err != nil {
			return -1, err
		}
		term.SetMaster(master)
	}

	log.Printf("console: %s", console)

	command := createCommand(container, console, rootfs, dataPath, os.Args[0], syncPipe.child, args)

	if err := term.Attach(command); err != nil {
		return -1, err
	}
	defer term.Close()

	if err := command.Start(); err != nil {
		return -1, err
	}

	// Now we passed the pipe to the child, close our side
	syncPipe.CloseChild()

	started, err := system.GetProcessStartTime(command.Process.Pid)
	if err != nil {
		return -1, err
	}

	// Do this before syncing with child so that no children
	// can escape the cgroup
	cleaner, err := SetupCgroups(container, command.Process.Pid)
	if err != nil {
		command.Process.Kill()
		command.Wait()
		return -1, err
	}
	if cleaner != nil {
		defer cleaner.Cleanup()
	}

	// Write user mappings while child is waiting
	if err := writeUserMappings(command.Process.Pid, container.UidMappings, container.GidMappings); err != nil {
		command.Process.Kill()
		command.Wait()
		return -1, err
	}
	log.Println("Wrote User Mappings.")

	var networkState network.NetworkState
	if err := InitializeNetworking(container, command.Process.Pid, syncPipe, &networkState); err != nil {
		command.Process.Kill()
		command.Wait()
		return -1, err
	}

	log.Printf("%+v", networkState)

	state := &libcontainer.State{
		InitPid:       command.Process.Pid,
		InitStartTime: started,
		NetworkState:  networkState,
	}

	if err := libcontainer.SaveState(dataPath, state); err != nil {
		command.Process.Kill()
		command.Wait()
		return -1, err
	}
	defer libcontainer.DeleteState(dataPath)



	// Start and run a helper process to setup the container
/*

	syncPipe2, err := NewSyncPipe()
	if err != nil {
		command.Process.Kill()
		command.Wait()
		return -1, err
	}
	defer syncPipe2.Close()

	setupCmd := setupCommand(container, console, rootfs, dataPath, os.Args[0], syncPipe2.child, args)
	log.Println("Before setup started")

	if err := setupCmd.Start(); err != nil {
		command.Process.Kill()
		command.Wait()
		return -1, err
	}
	log.Println("After setup started")

	// Now we passed the pipe to the child, close our side
	syncPipe2.CloseChild()
	log.Println("Before sending network")

	if err := syncPipe2.SendToChild(&networkState); err != nil {
		command.Process.Kill()
		command.Wait()
		setupCmd.Process.Kill()
		setupCmd.Wait()
		return -1, err
	}
	log.Println("After sending network")

	if err := syncPipe2.ReadFromChild(); err != nil {
		command.Process.Kill()
		command.Wait()
		setupCmd.Process.Kill()
		setupCmd.Wait()
		return -1, err
	}
	log.Println("After read child")

	if err := setupCmd.Wait(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			log.Println("Setup command failed.")
			command.Process.Kill()
			command.Wait()
			return -1, err
		}
	}
	log.Println("After waiting for setup")


	// Sync with original child
	if err := syncPipe.SendToChild(&networkState); err != nil {
		command.Process.Kill()
		command.Wait()
		return -1, err
	}
*/

	if err := syncPipe.ReadFromChild(); err != nil {
		command.Process.Kill()
		command.Wait()
		return -1, err
	}

	if startCallback != nil {
		startCallback()
	}

	if err := command.Wait(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return -1, err
		}
	}
	return command.ProcessState.Sys().(syscall.WaitStatus).ExitStatus(), nil
}

// DefaultCreateCommand will return an exec.Cmd with the Cloneflags set to the proper namespaces
// defined on the container's configuration and use the current binary as the init with the
// args provided
//
// console: the /dev/console to setup inside the container
// init: the program executed inside the namespaces
// root: the path to the container json file and information
// pipe: sync pipe to synchronize the parent and child processes
// args: the arguments to pass to the container to run as the user's program
func DefaultCreateCommand(container *libcontainer.Config, console, rootfs, dataPath, init string, pipe *os.File, args []string) *exec.Cmd {
	// get our binary name from arg0 so we can always reexec ourself
	env := []string{
		"console=" + console,
		"pipe=3",
		"data_path=" + dataPath,
	}

	/*
	   TODO: move user and wd into env
	   if user != "" {
	       env = append(env, "user="+user)
	   }
	   if workingDir != "" {
	       env = append(env, "wd="+workingDir)
	   }
	*/

	command := exec.Command(init, append([]string{"init"}, args...)...)
	// make sure the process is executed inside the context of the rootfs
	command.Dir = rootfs
	command.Env = append(os.Environ(), env...)

	system.SetCloneFlags(command, uintptr(GetNamespaceFlags(container.Namespaces)))
	command.SysProcAttr.Pdeathsig = syscall.SIGKILL
	command.ExtraFiles = []*os.File{pipe}

	return command
}

func DefaultSetupCommand(container *libcontainer.Config, console, rootfs, dataPath, init string, pipe *os.File, args []string) *exec.Cmd {
	// get our binary name from arg0 so we can always reexec ourself
	env := []string{
		"console=" + console,
		"pipe=3",
		"data_path=" + dataPath,
	}

	command := exec.Command(init, append([]string{"setup"}, args...)...)
	// make sure the process is executed inside the context of the rootfs
	command.Dir = rootfs
	command.Env = append(os.Environ(), env...)

	//command.SysProcAttr.Pdeathsig = syscall.SIGKILL
	command.ExtraFiles = []*os.File{pipe}

	return command
}

// SetupCgroups applies the cgroup restrictions to the process running in the container based
// on the container's configuration
func SetupCgroups(container *libcontainer.Config, nspid int) (cgroups.ActiveCgroup, error) {
	if container.Cgroups != nil {
		c := container.Cgroups
		if systemd.UseSystemd() {
			return systemd.Apply(c, nspid)
		}
		return fs.Apply(c, nspid)
	}
	return nil, nil
}

// InitializeNetworking creates the container's network stack outside of the namespace and moves
// interfaces into the container's net namespaces if necessary
func InitializeNetworking(container *libcontainer.Config, nspid int, pipe *SyncPipe, networkState *network.NetworkState) error {
	for _, config := range container.Networks {
		strategy, err := network.GetStrategy(config.Type)
		if err != nil {
			return err
		}
		if err := strategy.Create((*network.Network)(config), nspid, networkState); err != nil {
			return err
		}
	}
	log.Printf("NS: %+v", networkState)
	return pipe.SendToChild(networkState)
}

// GetNamespaceFlags parses the container's Namespaces options to set the correct
// flags on clone, unshare, and setns
func GetNamespaceFlags(namespaces map[string]bool) (flag int) {
	for key, enabled := range namespaces {
		if enabled {
			if ns := GetNamespace(key); ns != nil {
				flag |= ns.Value
			}
		}
	}
	return flag
}
