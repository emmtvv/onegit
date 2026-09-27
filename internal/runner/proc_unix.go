//go:build unix

package runner

import (
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroup runs the step in its own process group so cancellation
// reaches every child: SIGTERM first, SIGKILL ten seconds later.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid
		syscall.Kill(-pid, syscall.SIGTERM)
		go func() {
			time.Sleep(10 * time.Second)
			syscall.Kill(-pid, syscall.SIGKILL)
		}()
		return nil
	}
}
