// +build linux

package namespaces

import (
	"encoding/json"
	"log"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"syscall"

	"github.com/docker/libcontainer"
	"github.com/docker/libcontainer/cgroups"
	"github.com/docker/libcontainer/cgroups/fs"
	"github.com/docker/libcontainer/cgroups/systemd"
	"github.com/docker/libcontainer/network"
	"github.com/docker/libcontainer/system"
)

// TODO(vishh): This is part of the libcontainer API and it does much more than just namespaces related work.
// Move this to libcontainer package.
// Exec performs setup outside of a namespace so that a container can be
// executed.  Exec is a high level function for working with container namespaces.
func Exec(container *libcontainer.Config, stdin io.Reader, stdout, stderr io.Writer, console, dataPath string, args []string, createCommand CreateCommand, setupCommand SetupCommand, startCallback func()) (int, error) {
	var err error

	// create a pipe so that we can syncronize with the namespaced process and
	// pass the state and configuration to the child process
	parent, child, err := newInitPipe()
	if err != nil {
		return -1, err
	}
	defer parent.Close()

	command := createCommand(container, console, dataPath, os.Args[0], child, args)
	// Note: these are only used in non-tty mode
	// if there is a tty for the container it will be opened within the namespace and the
	// fds will be duped to stdin, stdiout, and stderr
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr

	log.Println("Starting command")
	if err := command.Start(); err != nil {
		child.Close()
		log.Println("Failed to launch command")
		return -1, err
	}
	child.Close()

	terminate := func(terr error) (int, error) {
		// TODO: log the errors for kill and wait
		command.Process.Kill()
		command.Wait()
		return -1, terr
	}

	started, err := system.GetProcessStartTime(command.Process.Pid)
	if err != nil {
		return terminate(err)
	}

	// Do this before syncing with child so that no children
	// can escape the cgroup
	cgroupPaths, err := SetupCgroups(container, command.Process.Pid)
	if err != nil {
		return terminate(err)
	}
	defer cgroups.RemovePaths(cgroupPaths)

	var networkState network.NetworkState
	log.Println("Initialzing networking.")
	if err := InitializeNetworking(container, command.Process.Pid, &networkState); err != nil {
		return terminate(err)
	}

	state := &libcontainer.State{
		InitPid:       command.Process.Pid,
		InitStartTime: started,
		NetworkState:  networkState,
		CgroupPaths:   cgroupPaths,
	}

	log.Println("Saving state.")
	if err := libcontainer.SaveState(dataPath, state); err != nil {
		return terminate(err)
	}
	defer libcontainer.DeleteState(dataPath)

	// Start the setup process to setup the init process
	log.Println("Starting setup")
	setupCmd := setupCommand(container, console, dataPath, os.Args[0])
	setupOut, _ := setupCmd.StderrPipe()
	err = setupCmd.Start()
        if err != nil {
		command.Process.Kill()
		command.Wait()
		log.Println("setup failed: %v", err)
		return -1, err
	}
	out, _ := ioutil.ReadAll(setupOut)
	log.Println("SETUP OUTPUT: %v", string(out))

	if err := setupCmd.Wait(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			command.Process.Kill()
			command.Wait()
			return -1, err
		}
	}
	log.Println("Setup return code", setupCmd.ProcessState.Sys().(syscall.WaitStatus).ExitStatus())

	// send the state to the container's init process then shutdown writes for the parent
	if err := json.NewEncoder(parent).Encode(networkState); err != nil {
		return terminate(err)
	}
	// shutdown writes for the parent side of the pipe
	if err := syscall.Shutdown(int(parent.Fd()), syscall.SHUT_WR); err != nil {
		return terminate(err)
	}

	// wait for the child process to fully complete and receive an error message
	// if one was encoutered
	var ierr *initError
	if err := json.NewDecoder(parent).Decode(&ierr); err != nil && err != io.EOF {
		return terminate(err)
	}
	if ierr != nil {
		return terminate(ierr)
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

// Converts IDMap to SysProcIDMap array and adds it to SysProcAttr.
func AddUidGidMappings(sys *syscall.SysProcAttr, container *libcontainer.Config) {
	if container.UidMappings != nil {
		sys.UidMappings = make([]syscall.SysProcIDMap, len(container.UidMappings))
		for i, um := range container.UidMappings {
			sys.UidMappings[i].ContainerID = um.ContainerID
			sys.UidMappings[i].HostID = um.HostID
			sys.UidMappings[i].Size = um.Size
		}
	}

	if container.GidMappings != nil {
		sys.GidMappings = make([]syscall.SysProcIDMap, len(container.GidMappings))
		for i, gm := range container.GidMappings {
			sys.GidMappings[i].ContainerID = gm.ContainerID
			sys.GidMappings[i].HostID = gm.HostID
			sys.GidMappings[i].Size = gm.Size
		}
	}
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
func DefaultCreateCommand(container *libcontainer.Config, console, dataPath, init string, pipe *os.File, args []string) *exec.Cmd {
	// get our binary name from arg0 so we can always reexec ourself
	env := []string{
		"console=" + console,
		"pipe=3",
		"data_path=" + dataPath,
	}

	command := exec.Command(init, append([]string{"init", "--"}, args...)...)
	// make sure the process is executed inside the context of the rootfs
	command.Dir = container.RootFs
	command.Env = append(os.Environ(), env...)

	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Cloneflags = uintptr(GetNamespaceFlags(container.Namespaces))

	command.SysProcAttr.Pdeathsig = syscall.SIGKILL
	command.ExtraFiles = []*os.File{pipe}

	for _, v := range container.Namespaces {
		if v.Name == "NEWUSER" {
			log.Println("Found user mappings.")
			if container.UidMappings != nil || container.GidMappings != nil {
				AddUidGidMappings(command.SysProcAttr, container)
			}

			// Default to root user when user namespaces are enabled.
			if command.SysProcAttr.Credential == nil {
				command.SysProcAttr.Credential = &syscall.Credential{}
			}
		}
	}

	return command
}

// DefaultSetupCommand will return an exec.Cmd that joins the init process to set it up.
//
// console: the /dev/console to setup inside the container
// init: the program executed inside the namespaces
// root: the path to the container json file and information
// args: the arguments to pass to the container to run as the user's program
func DefaultSetupCommand(container *libcontainer.Config, console, dataPath, init string) *exec.Cmd {
	// get our binary name from arg0 so we can always reexec ourself
	env := []string{
		"console=" + console,
		"data_path=" + dataPath,
	}

	log.Println("Console", console)
	if dataPath == "" {
		dataPath, _ = os.Getwd()
	}
	log.Println("DATAPATH", dataPath)

	if container.RootFs == "" {
		container.RootFs, _ = os.Getwd()
	}
	args := []string{dataPath, container.RootFs, console}

	command := exec.Command(init, append([]string{"exec", "--func", "setup", "--"}, args...)...)
	log.Println("(%+v)", command)

	// make sure the process is executed inside the context of the rootfs
	log.Println("ROOTFS: ", container.RootFs)
	command.Dir = container.RootFs
	command.Env = append(os.Environ(), env...)

	return command
}

// SetupCgroups applies the cgroup restrictions to the process running in the container based
// on the container's configuration
func SetupCgroups(container *libcontainer.Config, nspid int) (map[string]string, error) {
	if container.Cgroups != nil {
		c := container.Cgroups
		if systemd.UseSystemd() {
			return systemd.Apply(c, nspid)
		}
		return fs.Apply(c, nspid)
	}
	return map[string]string{}, nil
}

// InitializeNetworking creates the container's network stack outside of the namespace and moves
// interfaces into the container's net namespaces if necessary
func InitializeNetworking(container *libcontainer.Config, nspid int, networkState *network.NetworkState) error {
	for _, config := range container.Networks {
		strategy, err := network.GetStrategy(config.Type)
		if err != nil {
			return err
		}
		if err := strategy.Create((*network.Network)(config), nspid, networkState); err != nil {
			return err
		}
	}
	return nil
}
