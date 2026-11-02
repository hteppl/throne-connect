package xray

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// setProcAttrs keeps xray from opening its own console window.
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow, HideWindow: true}
}
