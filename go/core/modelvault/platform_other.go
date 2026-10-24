//go:build !darwin

package modelvault

import "os"

func platformVaultRoot(string) (string, error) { return "", ErrUnsupportedPlatform }

func platformLock(*os.File) error { return ErrUnsupportedPlatform }

func platformUnlock(*os.File) error { return ErrUnsupportedPlatform }
