//go:build unix

package pi

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// prepareCmdForKill puts the spawned child into its own process group so that
// the entire descendant tree can be terminated with a single signal aimed at
// the negative PID. Without this, cc-connect can only signal the direct child
// (the `pi` CLI), leaving any grandchildren (MCP server processes, tool
// subprocesses) as orphans after the parent is killed.
//
// Mirrors the pattern used by agent/claudecode/proc_unix.go and
// agent/codex/proc_unix.go.
func prepareCmdForKill(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// groupKillErrIsBenign reports whether a failed process-group kill can be ignored because the
// group is already gone. macOS reports EPERM rather than ESRCH for a group whose leader is a
// zombie that has not been reaped yet, so EPERM counts as benign only when the direct child can
// no longer be signalled either.
func groupKillErrIsBenign(err error, proc *os.Process) bool {
	if err == nil || errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return true
	}
	if errors.Is(err, syscall.EPERM) && proc != nil {
		if proc.Signal(syscall.Signal(0)) != nil {
			return true
		}
	}
	return false
}

// forceKillCmd SIGKILLs the entire process group rooted at cmd.
// Returns nil if the group is already gone.
func forceKillCmd(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	groupErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if groupKillErrIsBenign(groupErr, cmd.Process) {
		return nil
	}
	// A refused group kill must not leave the `pi` CLI itself running: fall back to
	// signalling the direct child before reporting a failure.
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) &&
		!errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill process group: %w (direct kill: %v)", groupErr, err)
	}
	return nil
}
