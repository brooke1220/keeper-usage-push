package push

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func assertPrivateSpoolPath(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		t.Fatalf("%s has no restrictive DACL: %v", path, err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	var hasUser bool
	const requiredAccess = windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.DELETE
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || (sid != user.User.Sid.String() && sid != "S-1-5-18") {
			t.Errorf("%s grants access outside the CPA account and SYSTEM: %s", path, sd.String())
		}
		if sid == user.User.Sid.String() && ace.Mask&requiredAccess == requiredAccess {
			hasUser = true
		}
	}
	if !hasUser {
		t.Errorf("%s does not grant read, write and delete access to the CPA account: %s", path, sd.String())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() && control&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("%s still inherits its parent directory's permissions", path)
	}
}

func makeSpoolPathPublic(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	inherit := ""
	if mode&0100 != 0 {
		inherit = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;" + inherit + ";FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}
