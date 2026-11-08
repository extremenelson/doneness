//go:build !linux

package gate

import "syscall"

// sysProcAttr puts the check in its own process group. Without Pdeathsig
// (Linux only), the inherited slot lock still keeps the machine-wide cap
// intact if the gate is killed: the slot stays held until the check exits.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
