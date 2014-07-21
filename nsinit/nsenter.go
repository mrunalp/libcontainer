package nsinit

import (
	"log"
	"os"

	"github.com/codegangsta/cli"
	"github.com/docker/libcontainer"
	"github.com/docker/libcontainer/namespaces"
)

var (
	setupMode = os.Getenv("setup")
	consolePath = os.Getenv("console")
	rootfs = os.Getenv("rootfs")

	nsenterCommand = cli.Command{
		Name:   "nsenter",
		Usage:  "init process for entering an existing namespace",
		Action: nsenterAction,
		Flags: []cli.Flag{
			cli.IntFlag{Name: "nspid"},
			cli.StringFlag{Name: "containerjson"},
		},
	}
)

func nsenterAction(context *cli.Context) {
	f, err := os.OpenFile("testlogfile", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("error opening file: %v", err)
	}
	defer f.Close()

	log.SetOutput(f)
	log.Println("This is a test log entry")

	args := context.Args()

	if len(args) == 0 {
		args = []string{"/bin/bash"}
	}

	container, err := loadContainerFromJson(context.String("containerjson"))
	if err != nil {
		log.Fatalf("unable to load container: %s", err)
	}

	nspid := context.Int("nspid")
	if nspid <= 0 {
		log.Fatalf("cannot enter into namespaces without valid pid: %q", nspid)
	}

	setup := false
	if setupMode != "" {
		setup = true
	}

	if rootfs != "" {
		if err := os.Chdir(rootfs); err != nil {
			log.Fatalf("Failed to change directory")
		}
	}

	var state *libcontainer.State
	if setup {
		state, err = libcontainer.GetState(dataPath)
		if err != nil && !os.IsNotExist(err) {
			log.Fatalf("unable to read state.json: %s", err)
		}
		if consolePath == "" {
			log.Fatalf("consolePath not set")
		}
	}

	log.Println("Setup: ", setup)
	log.Println("ConsolePath: ", consolePath)
	log.Println("rootfs: ", rootfs)

	if err := namespaces.NsEnter(container, nspid, args, setup, rootfs, consolePath, state); err != nil {
		log.Fatalf("failed to nsenter: %s", err)
	}
}
