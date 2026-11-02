//go:build !linux && !windows

package xray

import "os/exec"

func setProcAttrs(cmd *exec.Cmd) {}
