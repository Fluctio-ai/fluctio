//go:build !windows

package daemon

import (
	"os"
	"os/exec"
	"syscall"
)

func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func isCleanShutdown(exitErr *exec.ExitError) bool {
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
		if status.Signal() == syscall.SIGTERM || status.Signal() == syscall.SIGINT {
			return true
		}
	}
	return false
}

func signalProcess(proc *os.Process, sig string) error {
	switch sig {
	case "TERM":
		return proc.Signal(syscall.SIGTERM)
	case "KILL":
		return proc.Signal(syscall.SIGKILL)
	case "CHECK":
		return proc.Signal(syscall.Signal(0))
	case "RELOAD":
		return proc.Signal(syscall.SIGHUP)
	}
	return nil
}

// signalProcessGroup signals the whole process group the daemon runs in.
// `daemon start` spawns the wrapper with Setsid, making it a session and
// group leader whose pgid equals its pid — so a negative-pid signal
// reaches the wrapper AND every gateway child it spawned, instead of
// orphaning the gateway the way a single-process TERM did.
//
// No os.FindProcess here on purpose: on Linux it is a name lookup that
// cannot meaningfully fail for a live pid (existence is checked by the
// Kill itself), and the group signal targets -pid, not a process handle.
func signalProcessGroup(pid int, sig string) error {
	switch sig {
	case "TERM":
		return syscall.Kill(-pid, syscall.SIGTERM)
	case "KILL":
		return syscall.Kill(-pid, syscall.SIGKILL)
	case "CHECK":
		return syscall.Kill(-pid, syscall.Signal(0))
	}
	return nil
}
