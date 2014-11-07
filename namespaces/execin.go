// +build linux

package namespaces

import (
	"fmt"
	"log"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/docker/libcontainer"
	"github.com/docker/libcontainer/apparmor"
	"github.com/docker/libcontainer/cgroups"
	"github.com/docker/libcontainer/label"
	"github.com/docker/libcontainer/mount"
	"github.com/docker/libcontainer/security/restrict"
	"github.com/docker/libcontainer/syncpipe"
	"github.com/docker/libcontainer/system"
	"github.com/docker/libcontainer/utils"
)

// ExecIn reexec's the initPath with the argv 0 rewrite to "nsenter" so that it is able to run the
// setns code in a single threaded environment joining the existing containers' namespaces.
func ExecIn(container *libcontainer.Config, state *libcontainer.State, userArgs []string, initPath, action string,
	stdin io.Reader, stdout, stderr io.Writer, console string, startCallback func(*exec.Cmd)) (int, error) {

	args := []string{fmt.Sprintf("nsenter-%s", action), "--nspid", strconv.Itoa(state.InitPid)}

	if console != "" {
		args = append(args, "--console", console)
	}

	cmd := &exec.Cmd{
		Path: initPath,
		Args: append(args, append([]string{"--"}, userArgs...)...),
	}

	if filepath.Base(initPath) == initPath {
		if lp, err := exec.LookPath(initPath); err == nil {
			cmd.Path = lp
		}
	}

	pipe, err := syncpipe.NewSyncPipe()
	if err != nil {
		return -1, err
	}
	defer pipe.Close()

	// Note: these are only used in non-tty mode
	// if there is a tty for the container it will be opened within the namespace and the
	// fds will be duped to stdin, stdiout, and stderr
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	cmd.ExtraFiles = []*os.File{pipe.Child()}

	if err := cmd.Start(); err != nil {
		return -1, err
	}
	pipe.CloseChild()

	// Enter cgroups.
	if err := EnterCgroups(state, cmd.Process.Pid); err != nil {
		return -1, err
	}

	if err := pipe.SendToChild(container); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return -1, err
	}

	if startCallback != nil {
		startCallback(cmd)
	}

	if err := cmd.Wait(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return -1, err
		}
	}

	return cmd.ProcessState.Sys().(syscall.WaitStatus).ExitStatus(), nil
}

// Finalize expects that the setns calls have been setup and that is has joined an
// existing namespace
func FinalizeSetns(container *libcontainer.Config, args []string) error {
	// clear the current processes env and replace it with the environment defined on the container
	if err := LoadContainerEnvironment(container); err != nil {
		return err
	}

	if err := FinalizeNamespace(container); err != nil {
		return err
	}

	if err := apparmor.ApplyProfile(container.AppArmorProfile); err != nil {
		return fmt.Errorf("set apparmor profile %s: %s", container.AppArmorProfile, err)
	}

	if container.ProcessLabel != "" {
		if err := label.SetProcessLabel(container.ProcessLabel); err != nil {
			return err
		}
	}

	if err := system.Execv(args[0], args[0:], container.Env); err != nil {
		return err
	}

	panic("unreachable")
}

func SetupContainer(container *libcontainer.Config, args []string) error {
	lfile, err := os.OpenFile("mlog.txt", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalln("Failed to open log file: %v", err)
	}
	mLog := log.New(lfile, "SETUP: ", log.Ldate|log.Ltime|log.Lshortfile)
	consolePath := args[0]
	dataPath := args[1]

	mLog.Println("Console", consolePath)
	mLog.Println("dataPath", dataPath)

	rootfs, err := utils.ResolveRootfs(container.RootFs)
	if err != nil {
		return err
	}

	// clear the current processes env and replace it with the environment
	// defined on the container
	if err := LoadContainerEnvironment(container); err != nil {
		return err
	}

	state, err := libcontainer.GetState(dataPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("unable to read state.json %s", err)
	}

	if err := setupNetwork(container, &state.NetworkState); err != nil {
		return fmt.Errorf("setup networking %s", err)
	}

	if err := setupRoute(container); err != nil {
		return fmt.Errorf("setup route %s", err)
	}

	label.Init()

	if err := mount.InitializeMountNamespace(rootfs,
		consolePath,
		container.RestrictSys,
		(*mount.MountConfig)(container.MountConfig)); err != nil {
		return fmt.Errorf("setup mount namespace %s", err)
	}

	if container.Hostname != "" {
		if err := syscall.Sethostname([]byte(container.Hostname)); err != nil {
			return fmt.Errorf("sethostname %s", err)
		}
	}

	if err := apparmor.ApplyProfile(container.AppArmorProfile); err != nil {
		return fmt.Errorf("set apparmor profile %s: %s", container.AppArmorProfile, err)
	}

	if err := label.SetProcessLabel(container.ProcessLabel); err != nil {
		return fmt.Errorf("set process label %s", err)
	}

	// TODO: (crosbymichael) make this configurable at the Config level
	if container.RestrictSys {
		if err := restrict.Restrict("proc/sys", "proc/sysrq-trigger", "proc/irq", "proc/bus"); err != nil {
			return err
		}
	}

	return nil
}

func EnterCgroups(state *libcontainer.State, pid int) error {
	return cgroups.EnterPid(state.CgroupPaths, pid)
}
