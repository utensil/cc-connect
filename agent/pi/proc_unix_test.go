//go:build unix

package pi

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// Regression: macOS reports EPERM for a process group whose leader is a zombie that has not been
// reaped yet. Treating that as a hard failure made killRPC() log "operation not permitted"
// warnings on every daemon shutdown (12 occurrences in the live log).
func TestGroupKillErrIsBenign(t *testing.T) {
	alive := exec.Command("/bin/sleep", "5")
	prepareCmdForKill(alive)
	if err := alive.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = alive.Process.Kill()
		_, _ = alive.Process.Wait()
	}()

	done := exec.Command("true")
	prepareCmdForKill(done)
	if err := done.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}

	cases := []struct {
		name string
		err  error
		proc *os.Process
		want bool
	}{
		{"nil error", nil, alive.Process, true},
		{"process done", os.ErrProcessDone, done.Process, true},
		{"esrch", syscall.ESRCH, alive.Process, true},
		{"eperm on an exited child", syscall.EPERM, done.Process, true},
		{"eperm on a live child", syscall.EPERM, alive.Process, false},
		{"generic error", errors.New("boom"), alive.Process, false},
		{"eperm without a process", syscall.EPERM, nil, false},
	}
	for _, c := range cases {
		if got := groupKillErrIsBenign(c.err, c.proc); got != c.want {
			t.Errorf("groupKillErrIsBenign(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestForceKillCmd_KillsGroupThenToleratesAnExitedChild(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	prepareCmdForKill(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := forceKillCmd(cmd); err != nil {
		t.Fatalf("forceKillCmd on a live child = %v, want nil", err)
	}
	_, _ = cmd.Process.Wait() // reap so the second call sees a truly finished process
	if err := forceKillCmd(cmd); err != nil {
		t.Fatalf("forceKillCmd on an exited child = %v, want nil", err)
	}
}
