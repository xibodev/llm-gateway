//go:build windows

package operations

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func restrictPath(path string, directory bool) error {
	descriptor, err := privateSecurityDescriptor(directory)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	)
}

func privateSecurityDescriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	return windows.SecurityDescriptorFromString(fmt.Sprintf(
		"D:P(A;%s;FA;;;SY)(A;%s;FA;;;%s)",
		inheritance, inheritance, user.User.Sid.String(),
	))
}

func makeSecureTempDir(pattern string) (string, error) {
	descriptor, err := privateSecurityDescriptor(true)
	if err != nil {
		return "", err
	}
	attributes := windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor,
	}
	for attempt := 0; attempt < 100; attempt++ {
		name, err := randomTempName(pattern)
		if err != nil {
			return "", err
		}
		path := filepath.Join(os.TempDir(), name)
		pathPointer, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return "", err
		}
		if err := windows.CreateDirectory(pathPointer, &attributes); err == nil {
			return path, nil
		} else if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return "", err
		}
	}
	return "", fmt.Errorf("create secure temporary directory: exhausted unique names")
}

func createSecureTempFile(dir, pattern string) (*os.File, error) {
	for attempt := 0; attempt < 100; attempt++ {
		name, err := randomTempName(pattern)
		if err != nil {
			return nil, err
		}
		file, err := createSecureFile(filepath.Join(dir, name))
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, windows.ERROR_FILE_EXISTS) && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("create secure temporary file: exhausted unique names")
}

func createSecureFile(path string) (*os.File, error) {
	descriptor, err := privateSecurityDescriptor(false)
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor,
	}
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPointer, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, &attributes,
		windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func randomTempName(pattern string) (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	suffix := hex.EncodeToString(value)
	if strings.Contains(pattern, "*") {
		return strings.Replace(pattern, "*", suffix, 1), nil
	}
	return pattern + suffix, nil
}

func durableRename(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
}

func replaceFile(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
