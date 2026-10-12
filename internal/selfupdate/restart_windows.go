package selfupdate

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// supervisedEnv marks a child started by Restart; such a child does not
	// start another one but asks its parent with exitRestart.
	supervisedEnv = "JWLSFM_SUPERVISED"
	// exitRestart never leaves the process pair: the supervisor consumes it.
	exitRestart = 3
)

// Restart runs the updated executable with the same arguments. Windows has no
// exec, so the old process stays as a supervisor: it waits for the child,
// starts it again after further updates and finally returns the child's exit
// code. The child shares stdout/stderr (log redirection keeps working) and is
// killed together with the supervisor (job object), so stopping the scheduled
// task stops both.
func (u *Updater) Restart() (exitCode int, err error) {
	if os.Getenv(supervisedEnv) != "" {
		u.Log.Info("restarting with the new version")
		return exitRestart, nil
	}
	job, err := killOnCloseJob()
	if err != nil {
		u.Log.Warn("cannot tie the restarted process to this one", "err", err)
	}
	for {
		u.Log.Info("restarting with the new version")
		cmd := exec.Command(u.Exe, os.Args[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Env = append(os.Environ(), supervisedEnv+"=1")
		if err := cmd.Start(); err != nil {
			return 1, fmt.Errorf("restart: %w", err)
		}
		if job != 0 {
			if err := assignToJob(job, cmd.Process.Pid); err != nil {
				u.Log.Warn("cannot tie the restarted process to this one", "err", err)
			}
		}
		err := cmd.Wait()
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			return 1, fmt.Errorf("restart: %w", err)
		}
		if code := cmd.ProcessState.ExitCode(); code != exitRestart {
			return code, nil
		}
	}
}

func killOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	// the handle stays open for the lifetime of the process on purpose
	return job, nil
}

func assignToJob(job windows.Handle, pid int) error {
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(p)
	return windows.AssignProcessToJobObject(job, p)
}
