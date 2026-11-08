package gate

import "syscall"

// sysProcAttr puts the check in its own process group and, on Linux, asks
// the kernel to kill it if the gate dies.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
