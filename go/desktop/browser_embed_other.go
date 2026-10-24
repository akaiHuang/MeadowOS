//go:build !darwin

package main

const desktopBrowserEmbedSupported = false

func newDesktopBrowserEmbedImpl(_ *desktopBrowserEmbed) desktopBrowserEmbedImpl {
	return unavailableBrowserEmbed{}
}

type unavailableBrowserEmbed struct{}

func (unavailableBrowserEmbed) setFrame(bool, float64, float64, float64, float64) {}
func (unavailableBrowserEmbed) navigate(string) error {
	return errDesktopBrowserEmbedUnavailable
}
func (unavailableBrowserEmbed) session() (string, string, bool) { return "", "", false }
func (unavailableBrowserEmbed) eval(string) (string, error) {
	return "", errDesktopBrowserEmbedUnavailable
}
func (unavailableBrowserEmbed) dom() (string, error) { return "", errDesktopBrowserEmbedUnavailable }
func (unavailableBrowserEmbed) console(int) []map[string]string {
	return nil
}
func (unavailableBrowserEmbed) screenshot() ([]byte, error) {
	return nil, errDesktopBrowserEmbedUnavailable
}
func (unavailableBrowserEmbed) back()    {}
func (unavailableBrowserEmbed) forward() {}
func (unavailableBrowserEmbed) reload()  {}
func (unavailableBrowserEmbed) close()   {}
