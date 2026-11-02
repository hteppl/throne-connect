package xray

import (
	"os/exec"
	"syscall"
)

// setProcAttrs makes the kernel kill xray if throne-connect dies, so a crash
// never leaves an orphan holding the ports.
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
