package namespaces

import (
	"os"
	"os/exec"

	"github.com/docker/libcontainer"
)

type CreateCommand func(container *libcontainer.Config, console, rootfs, dataPath, init string, childPipe *os.File, args []string) *exec.Cmd
type SetupCommand func(container *libcontainer.Config, console, rootfs, dataPath, init string, args []string) *exec.Cmd
