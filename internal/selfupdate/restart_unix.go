//go:build !windows

package selfupdate

import (
	"fmt"
	"os"
	"syscall"
)

// Restart replaces the running process with the updated executable, keeping
// arguments, environment and process id (service managers notice nothing).
// It only returns on failure.
func (u *Updater) Restart() (exitCode int, err error) {
	u.Log.Info("restarting with the new version")
	if err := syscall.Exec(u.Exe, os.Args, os.Environ()); err != nil {
		return 1, fmt.Errorf("restart: %w", err)
	}
	return 0, nil
}
