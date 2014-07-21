package nsinit

import (
	_ "fmt"
	"log"
	"os"
	_ "os/exec"
	_ "runtime"
	_ "strings"

	"github.com/codegangsta/cli"
	"github.com/docker/libcontainer"
	"github.com/docker/libcontainer/namespaces"
)

var (
	cons   = os.Getenv("console")

	setupCommand = cli.Command{
		Name:   "setup",
		Usage:  "setup the container",
		Action: setupAction,
	}
)

func setupAction(context *cli.Context) {
	var exitCode int

	container, err := loadContainer()
	if err != nil {
		log.Fatal(err)
	}

	state, err := libcontainer.GetState(dataPath)
	if err != nil && !os.IsNotExist(err) {
		log.Fatalf("unable to read state.json: %s", err)
	}

	if state == nil {
		log.Fatalf("Empty state")
	}

	rootfs, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	if state != nil {
		if err := namespaces.SetupContainer(container, rootfs, cons, state); err != nil {
			log.Fatalf("Failed to setup container: %s", err)
		}
	}

	os.Exit(exitCode)
}

