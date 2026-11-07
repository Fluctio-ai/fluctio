//go:build windows

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func setSysProcAttr(cmd *exec.Cmd) {}

func isCleanShutdown(exitErr *exec.ExitError) bool {
	return false
}

func signalProcess(proc *os.Process, sig string) error {
	switch sig {
	case "TERM", "KILL":
		return proc.Kill()
	case "CHECK":
		// On Windows, just try to open the process
		return nil
	case "RELOAD":
		// SIGHUP is not deliverable on Windows. Callers fall back to a
		// hint asking the operator to restart the gateway.
		return errReloadUnsupported
	}
	return nil
}

// signalProcessGroup tree-kills the daemon on Windows: taskkill /T /F
// terminates the wrapper and every gateway child it spawned in one call.
// The old single-process Kill orphaned the gateway, which kept serving
// while stop reported success.
func signalProcessGroup(pid int, sig string) error {
	switch sig {
	case "TERM", "KILL":
		// taskkill /T walks the child tree; /F forces termination. The
		// tree covers the wrapper and its gateway child on both counts,
		// matching the Unix group-kill semantics.
		out, err := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").CombinedOutput()
		if err != nil {
			return fmt.Errorf("taskkill %d: %w: %s", pid, err, strings.TrimSpace(string(out)))
		}
		return nil
	case "CHECK":
		// "Is alive" probes use isProcessAlive in daemon.go; nothing
		// signal-shaped to do here.
		return nil
	}
	return nil
}

// errReloadUnsupported is returned by signalProcess on Windows so the
// CLI can detect "no graceful reload available here" and degrade.
var errReloadUnsupported = winReloadErr("reload via signal is not supported on Windows")

type winReloadErr string

func (e winReloadErr) Error() string { return string(e) }
