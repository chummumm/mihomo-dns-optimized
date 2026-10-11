package route

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

type configFileMetadata struct {
	identity string
	security *windows.SECURITY_DESCRIPTOR
}

func openConfigFile(path string) (*os.File, error) { return os.Open(path) }

func readConfigFileMetadata(file *os.File, _ os.FileInfo) (configFileMetadata, error) {
	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &identity); err != nil {
		return configFileMetadata{}, err
	}
	security, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return configFileMetadata{}, err
	}
	return configFileMetadata{
		identity: fmt.Sprintf("%d:%d:%d:%s", identity.VolumeSerialNumber, identity.FileIndexHigh, identity.FileIndexLow, security.String()),
		security: security,
	}, nil
}

func preserveConfigFileMetadata(file *os.File, info os.FileInfo, metadata configFileMetadata) error {
	owner, _, err := metadata.security.Owner()
	if err != nil {
		return err
	}
	group, _, err := metadata.security.Group()
	if err != nil {
		return err
	}
	dacl, _, err := metadata.security.DACL()
	if err != nil {
		return err
	}
	control, _, err := metadata.security.Control()
	if err != nil {
		return err
	}
	flags := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.GROUP_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		flags |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		flags |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	// Preserve access controls instead of silently inheriting broader access from
	// the directory. If the service lacks permission, keep the original intact.
	if err := windows.SetNamedSecurityInfo(file.Name(), windows.SE_FILE_OBJECT, flags, owner, group, dacl, nil); err != nil {
		return err
	}
	return file.Chmod(info.Mode())
}
