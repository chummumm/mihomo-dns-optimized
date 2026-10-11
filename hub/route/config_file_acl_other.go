//go:build !linux && !windows

package route

import "os"

func readConfigFileAccessACL(*os.File) ([]byte, error) { return nil, nil }

func preserveConfigFileAccessACL(*os.File, []byte) error { return nil }
