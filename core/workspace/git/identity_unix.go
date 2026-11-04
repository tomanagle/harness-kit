//go:build !windows && !js && !wasip1

package git

import (
	"fmt"
	"os"
	"syscall"
)

func directoryIdentity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return "", fmt.Errorf("git: directory identity unavailable for %s", path)
	}
	return fmt.Sprintf("unix:%x:%x", stat.Dev, stat.Ino), nil
}
