//go:build !windows && !linux

package nest

import "syscall"

func setDeathSignal(a *syscall.SysProcAttr) {}
