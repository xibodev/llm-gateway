//go:build windows

package operations

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSecureTemporaryDirectoryUsesProtectedACL(t *testing.T) {
	dir, err := makeSecureTempDir("llmgw-acl-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	descriptor, err := windows.GetNamedSecurityInfo(
		dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("temporary directory DACL is not protected: %#x", control)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil || dacl.AceCount != 2 {
		t.Fatalf("temporary directory ACE count=%v, want system and current user", dacl)
	}
}
