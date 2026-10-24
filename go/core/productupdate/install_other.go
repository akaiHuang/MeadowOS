//go:build !darwin

package productupdate

import "context"

type unsupportedInstaller struct{}

func NewPlatformInstaller(_ SystemInstaller, _ CommandRunner) Installer {
	return unsupportedInstaller{}
}

func (unsupportedInstaller) VerifyAndHandoff(context.Context, string, Artifact, string) (HandoffReceipt, error) {
	return HandoffReceipt{}, contractError("INSTALL_PLATFORM", "authenticated package installation is only implemented on Darwin", nil)
}
