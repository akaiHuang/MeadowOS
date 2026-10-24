//go:build !darwin || !cgo

package textlocale

// ToTraditionalChinese is a no-op where the macOS ICU transform is absent.
// The model-level zh-TW instruction remains active on those development hosts.
func ToTraditionalChinese(value string) string { return value }
