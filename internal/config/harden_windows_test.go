//go:build windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// everyoneSID / usersSID are the two trustees whose presence would silently
// undo the whole feature. They are spelled as SID strings rather than derived
// from the code under test on purpose.
const (
	everyoneSID = "S-1-1-0"      // Everyone
	usersSID    = "S-1-5-32-545" // BUILTIN\Users
)

// wantTrustees derives the expected trustee set independently of
// ownerOnlyTrustees: the object's owner read straight from the descriptor,
// the process token user, SYSTEM, and BUILTIN\Administrators. A test that
// asked ownerOnlyTrustees what to expect would pass just as green if that
// function started returning Everyone.
func wantTrustees(t *testing.T, path string) []*windows.SID {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s, OWNER) error = %v", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatalf("Owner() error = %v", err)
	}

	user, err := currentProcessUser()
	if err != nil {
		t.Fatalf("currentProcessUser() error = %v", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(SYSTEM) error = %v", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(Administrators) error = %v", err)
	}

	want := make([]*windows.SID, 0, 4)
	for _, sid := range []*windows.SID{owner, user, system, admins} {
		if sid != nil && !containsSID(want, sid) {
			want = append(want, sid)
		}
	}
	return want
}

// TestHardenPermissionsProtectsDirAndFiles is the Windows counterpart of the
// Unix mode-bit tests: after HardenPermissions, --dir and the state/log files
// under it must carry a protected (non-inheriting) DACL naming exactly the
// owner-only trustee set and nobody else.
func TestHardenPermissionsProtectsDirAndFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scrim")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll setup error = %v", err)
	}
	statePath := filepath.Join(dir, "daemon.json")
	logPath := filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(statePath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("writing state file: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("log\n"), 0o600); err != nil {
		t.Fatalf("writing log file: %v", err)
	}

	cfg := Config{Dir: dir, Host: "127.0.0.1", Port: 7777}
	if err := cfg.HardenPermissions(); err != nil {
		t.Fatalf("HardenPermissions() error = %v", err)
	}

	tests := []struct {
		name        string
		path        string
		wantInherit uint8
	}{
		{"dir", dir, windows.CONTAINER_INHERIT_ACE | windows.OBJECT_INHERIT_ACE},
		{"state file", statePath, 0},
		{"log file", logPath, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertProtectedDACL(t, tt.path)
			aces := readACEs(t, tt.path)
			assertTrustees(t, aces, wantTrustees(t, tt.path))
			assertNoOpenTrustees(t, aces)
			for i, ace := range aces {
				if got := ace.Header.AceFlags & windows.VALID_INHERIT_FLAGS; got != tt.wantInherit {
					t.Errorf("ace %d inheritance flags = %#x, want %#x", i, got, tt.wantInherit)
				}
			}
		})
	}
}

// TestHardenPermissionsPropagatesToNewDescendants is the end-to-end proof of
// the inheritance claim README.md makes: a canvas file created after the dir
// was hardened must come up with the same trustee set, by inheritance,
// without scrim touching it.
func TestHardenPermissionsPropagatesToNewDescendants(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scrim")
	cfg := Config{Dir: dir, Host: "127.0.0.1", Port: 7777}
	if err := cfg.HardenPermissions(); err != nil {
		t.Fatalf("HardenPermissions() error = %v", err)
	}

	canvasDir := filepath.Join(dir, "canvases", "x")
	if err := os.MkdirAll(canvasDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", canvasDir, err)
	}
	canvasFile := filepath.Join(canvasDir, "index.html")
	if err := os.WriteFile(canvasFile, []byte("<p>hi</p>"), 0o600); err != nil {
		t.Fatalf("writing canvas file: %v", err)
	}

	for _, path := range []string{canvasDir, canvasFile} {
		aces := readACEs(t, path)
		assertTrustees(t, aces, wantTrustees(t, path))
		assertNoOpenTrustees(t, aces)
		for i, ace := range aces {
			if ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
				t.Errorf("%s ace %d flags = %#x, want INHERITED_ACE set (the dir ACL did not propagate)",
					path, i, ace.Header.AceFlags)
			}
		}
	}
}

// TestHardenPermissionsSecondPassDoesNotRewrite pins the conditional write:
// once a dir is hardened, HardenPermissions must recognize the existing DACL
// and leave it alone. An unconditional SetNamedSecurityInfo on a container
// re-propagates to every descendant, and this runs on every CLI verb -- so
// "no write" is the behaviour, not just "same result".
func TestHardenPermissionsSecondPassDoesNotRewrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scrim")
	cfg := Config{Dir: dir, Host: "127.0.0.1", Port: 7777}
	if err := cfg.HardenPermissions(); err != nil {
		t.Fatalf("first HardenPermissions() error = %v", err)
	}

	sd, err := windows.GetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s) error = %v", dir, err)
	}
	if !daclMatches(sd, wantTrustees(t, dir), windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT) {
		t.Fatal("daclMatches() = false right after hardening; every later verb would rewrite the whole subtree")
	}

	before := len(readACEs(t, dir))
	if err := cfg.HardenPermissions(); err != nil {
		t.Fatalf("second HardenPermissions() error = %v", err)
	}
	if after := len(readACEs(t, dir)); after != before {
		t.Errorf("ace count after second pass = %d, want %d (unchanged)", after, before)
	}
}

// TestDaclMatchesRejectsWrongShapes is the negative half of the conditional
// write: anything that isn't already exactly our DACL must still be written.
func TestDaclMatchesRejectsWrongShapes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scrim")
	cfg := Config{Dir: dir, Host: "127.0.0.1", Port: 7777}
	if err := cfg.HardenPermissions(); err != nil {
		t.Fatalf("HardenPermissions() error = %v", err)
	}
	sd, err := windows.GetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s) error = %v", dir, err)
	}
	want := wantTrustees(t, dir)

	everyone, err := windows.StringToSid(everyoneSID)
	if err != nil {
		t.Fatalf("StringToSid(%s) error = %v", everyoneSID, err)
	}
	withEveryone := append(append([]*windows.SID{}, want...), everyone)

	tests := []struct {
		name        string
		sd          *windows.SECURITY_DESCRIPTOR
		want        []*windows.SID
		inheritance uint32
	}{
		{"nil descriptor", nil, want, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT},
		{"empty trustee set", sd, nil, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT},
		{"wrong inheritance", sd, want, windows.NO_INHERITANCE},
		{"extra trustee wanted", sd, withEveryone, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT},
		{"fewer trustees wanted", sd, want[:len(want)-1], windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if daclMatches(tt.sd, tt.want, tt.inheritance) {
				t.Error("daclMatches() = true, want false")
			}
		})
	}
}

// TestHardenPermissionsFreshDirCreatesIt confirms HardenPermissions creates a
// --dir that doesn't exist yet, the same as the Unix path.
func TestHardenPermissionsFreshDirCreatesIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scrim-fresh")
	cfg := Config{Dir: dir, Host: "127.0.0.1", Port: 7777}
	if err := cfg.HardenPermissions(); err != nil {
		t.Fatalf("HardenPermissions() error = %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("os.Stat(%s) = %v, %v; want an existing directory", dir, fi, err)
	}
	assertProtectedDACL(t, dir)
}

// TestHardenFileMissingIsNotAnError pins the os.ErrNotExist tolerance the
// Unix implementation gets for free from os.Chmod: a state or log file the
// daemon hasn't written yet is nothing to tighten, not a failure.
func TestHardenFileMissingIsNotAnError(t *testing.T) {
	base := t.TempDir()
	tests := []struct {
		name string
		path string
	}{
		// ERROR_FILE_NOT_FOUND: the directory exists, the file doesn't.
		{"missing file", filepath.Join(base, "daemon.json")},
		// ERROR_PATH_NOT_FOUND: a parent component doesn't exist either.
		{"missing parent", filepath.Join(base, "absent", "daemon.json")},
		// ERROR_BAD_NETPATH: a UNC path whose server doesn't resolve. Only
		// os.ErrNotExist covers this one, which is why isNotExist checks it
		// rather than an explicit errno list.
		{"unresolvable unc path", `\\scrim-test-no-such-host\share\daemon.json`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := hardenFile(tt.path); err != nil {
				t.Errorf("hardenFile(%s) error = %v, want nil", tt.path, err)
			}
		})
	}
}

// TestDedupeSIDs confirms the trustee list collapses by SID equality rather
// than pointer identity (the object owner is usually also the process user,
// which would otherwise produce a redundant ACE) and drops the nil a
// descriptor-less object yields.
func TestDedupeSIDs(t *testing.T) {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(SYSTEM) error = %v", err)
	}
	// A distinct *SID value for the same well-known SID -- pointer identity
	// must not be what dedupeSIDs keys on.
	systemAgain, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(SYSTEM) error = %v", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(Administrators) error = %v", err)
	}

	got := dedupeSIDs(nil, system, systemAgain, admins, nil, admins)
	want := []*windows.SID{system, admins}
	if len(got) != len(want) {
		t.Fatalf("dedupeSIDs() returned %d sids, want %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].Equals(want[i]) {
			t.Errorf("sid %d = %s, want %s", i, got[i], want[i])
		}
	}
}

// TestOwnerOnlyTrusteesPolicy pins the trustee policy against independently
// derived SIDs: the process user (so a written DACL always names the account
// scrim runs as), SYSTEM, and BUILTIN\Administrators are present; Everyone
// and BUILTIN\Users are not.
func TestOwnerOnlyTrusteesPolicy(t *testing.T) {
	dir := t.TempDir()
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s) error = %v", dir, err)
	}
	got, err := ownerOnlyTrustees(sd)
	if err != nil {
		t.Fatalf("ownerOnlyTrustees() error = %v", err)
	}

	user, err := currentProcessUser()
	if err != nil {
		t.Fatalf("currentProcessUser() error = %v", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(SYSTEM) error = %v", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(Administrators) error = %v", err)
	}
	for _, want := range []*windows.SID{user, system, admins} {
		if !containsSID(got, want) {
			t.Errorf("trustees %v missing %s", sidStrings(got), want)
		}
	}

	for _, str := range []string{everyoneSID, usersSID} {
		open, err := windows.StringToSid(str)
		if err != nil {
			t.Fatalf("StringToSid(%s) error = %v", str, err)
		}
		if containsSID(got, open) {
			t.Errorf("trustees %v include the open trustee %s", sidStrings(got), open)
		}
	}

	if len(got) > 4 {
		t.Errorf("trustees = %v, want at most 4 (owner, process user, SYSTEM, Administrators)", sidStrings(got))
	}
}

func assertProtectedDACL(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s) error = %v", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("Control() error = %v", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("dacl of %s is not protected (control = %#x); inherited ACEs can leak back in", path, control)
	}
	if control&windows.SE_DACL_PRESENT == 0 {
		t.Errorf("dacl of %s is absent (control = %#x), which grants everyone access", path, control)
	}
}

// readACEs returns path's DACL entries. The SID each one names is read the
// way x/sys's own tests do it: the ACE is a variable-length struct whose SID
// starts at SidStart.
func readACEs(t *testing.T, path string) []*windows.ACCESS_ALLOWED_ACE {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s) error = %v", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("DACL() error = %v", err)
	}
	if dacl == nil {
		t.Fatalf("dacl of %s is nil", path)
	}
	aces := make([]*windows.ACCESS_ALLOWED_ACE, dacl.AceCount)
	for i := uint16(0); i < dacl.AceCount; i++ {
		if err := windows.GetAce(dacl, uint32(i), &aces[i]); err != nil {
			t.Fatalf("GetAce(%d) error = %v", i, err)
		}
	}
	return aces
}

// assertTrustees checks the DACL names exactly want, with every ACE an
// allow-full-control entry.
func assertTrustees(t *testing.T, aces []*windows.ACCESS_ALLOWED_ACE, want []*windows.SID) {
	t.Helper()
	got := aceSIDs(aces)
	if len(aces) != len(want) {
		t.Fatalf("dacl has %d aces (%v), want %d (%v)", len(aces), sidStrings(got), len(want), sidStrings(want))
	}
	for i, ace := range aces {
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Errorf("ace %d type = %d, want ACCESS_ALLOWED_ACE_TYPE", i, ace.Header.AceType)
		}
		// SetEntriesInAcl maps GENERIC_ALL to the object's specific rights, so
		// this is the mask a full-control ACE reads back with.
		if ace.Mask&fileFullControl != fileFullControl {
			t.Errorf("ace %d mask = %#x, want full control (%#x) set", i, ace.Mask, fileFullControl)
		}
	}
	for _, sid := range want {
		if !containsSID(got, sid) {
			t.Errorf("dacl %v missing trustee %s", sidStrings(got), sid)
		}
	}
	for _, sid := range got {
		if !containsSID(want, sid) {
			t.Errorf("dacl grants unexpected trustee %s", sid)
		}
	}
}

// assertNoOpenTrustees states the security property directly: no ACE may name
// a trustee that would leave the data readable by other accounts on the host.
func assertNoOpenTrustees(t *testing.T, aces []*windows.ACCESS_ALLOWED_ACE) {
	t.Helper()
	got := aceSIDs(aces)
	for _, str := range []string{everyoneSID, usersSID} {
		open, err := windows.StringToSid(str)
		if err != nil {
			t.Fatalf("StringToSid(%s) error = %v", str, err)
		}
		if containsSID(got, open) {
			t.Errorf("dacl %v grants the open trustee %s", sidStrings(got), open)
		}
	}
}

func aceSIDs(aces []*windows.ACCESS_ALLOWED_ACE) []*windows.SID {
	out := make([]*windows.SID, 0, len(aces))
	for _, ace := range aces {
		out = append(out, (*windows.SID)(unsafe.Pointer(&ace.SidStart)))
	}
	return out
}

func containsSID(sids []*windows.SID, want *windows.SID) bool {
	return indexOfSID(sids, want) >= 0
}

func sidStrings(sids []*windows.SID) []string {
	out := make([]string, 0, len(sids))
	for _, sid := range sids {
		out = append(out, sid.String())
	}
	return out
}

// TestIsACLUnsupportedClassification pins which errnos are treated as "this
// filesystem cannot hold an ACL" (warn once, keep running) versus a real
// failure (abort startup). Over-tolerating here silently ships an unhardened
// data directory, so the negative cases matter more than the positive ones.
func TestIsACLUnsupportedClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"not supported", windows.ERROR_NOT_SUPPORTED, true},
		{"call not implemented", windows.ERROR_CALL_NOT_IMPLEMENTED, true},
		{"invalid function (FAT/exFAT)", windows.ERROR_INVALID_FUNCTION, true},
		{"wrapped invalid function", fmt.Errorf("applying an ACL: %w", windows.ERROR_INVALID_FUNCTION), true},
		{"access denied stays fatal", windows.ERROR_ACCESS_DENIED, false},
		{"not found stays fatal", windows.ERROR_FILE_NOT_FOUND, false},
		{"invalid parameter stays fatal", windows.ERROR_INVALID_PARAMETER, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isACLUnsupported(tt.err); got != tt.want {
				t.Errorf("isACLUnsupported(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
