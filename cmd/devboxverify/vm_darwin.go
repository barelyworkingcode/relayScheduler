//go:build darwin

package main

import "syscall"

// hostIsVM deliberately has no override: a marker restored onto a host from
// a backup must still refuse.
func hostIsVM() (bool, error) {
	v, err := syscall.SysctlUint32("kern.hv_vmm_present")
	if err != nil {
		return false, err
	}
	return v == 1, nil
}
