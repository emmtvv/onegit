//go:build !unix

package runner

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}
