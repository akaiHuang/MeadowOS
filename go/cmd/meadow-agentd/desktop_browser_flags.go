package main

import (
	"flag"

	"github.com/hey-meadow/meadow-code/go/core/desktopbrowserrpc"
)

func desktopBrowserDescriptorFlags() (*int, *int) {
	return flag.Int("desktop-browser-fd", 0, "inherited Desktop Browser descriptor (fixed FD 3)"), flag.Int("desktop-browser-secret-fd", 0, "inherited Desktop Browser peer-secret descriptor (fixed FD 4)")
}

func configureDesktopBrowserDescriptors(config *startupConfig, fd, secretFD int) error {
	if err := desktopbrowserrpc.ValidateDescriptors(fd, secretFD); err != nil {
		return err
	}
	config.browserFD = uintptr(fd)
	config.browserSecretFD = uintptr(secretFD)
	return nil
}
