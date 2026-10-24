package tools

import "runtime"

// commandShell preserves the macOS command contract while using the shell
// shipped by the supported Ubuntu/Linux runtime. Never inherit a host SHELL
// path: a macOS launch environment can name a binary absent from the guest.
func commandShell() string {
	if runtime.GOOS == "linux" {
		return "/bin/bash"
	}
	return "/bin/zsh"
}
