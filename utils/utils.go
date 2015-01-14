package utils

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"path/filepath"
	"strconv"
	"syscall"
)

// GenerateRandomName returns a new name joined with a prefix.  This size
// specified is used to truncate the randomly generated value
func GenerateRandomName(prefix string, size int) (string, error) {
	id := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, id); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(id)[:size], nil
}

// ResolveRootfs ensures that the current working directory is
// not a symlink and returns the absolute path to the rootfs
func ResolveRootfs(uncleanRootfs string) (string, error) {
	rootfs, err := filepath.Abs(uncleanRootfs)
	if err != nil {
		return "", fmt.Errorf("filepath abs: %v", err)
	}
	str, err := filepath.EvalSymlinks(rootfs)
	return str, fmt.Errorf("filepath evalsymlinks: %v", err)
}

func CloseExecFrom(minFd int) error {
	fdList, err := ioutil.ReadDir("/proc/self/fd")
	if err != nil {
		return err
	}
	for _, fi := range fdList {
		fd, err := strconv.Atoi(fi.Name())
		if err != nil {
			// ignore non-numeric file names
			continue
		}

		if fd < minFd {
			// ignore descriptors lower than our specified minimum
			continue
		}

		// intentionally ignore errors from syscall.CloseOnExec
		syscall.CloseOnExec(fd)
		// the cases where this might fail are basically file descriptors that have already been closed (including and especially the one that was created when ioutil.ReadDir did the "opendir" syscall)
	}
	return nil
}
