//go:build windows

package config

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/jedwards1230/scrim/internal/logging"
)

// fileFullControl is FILE_ALL_ACCESS: the specific-rights mask a file or
// directory object's generic mapping turns GENERIC_ALL into.
//
// The mapping is applied by the kernel when the descriptor is set on the object,
// not by SetEntriesInAcl -- SetEntriesInAcl copies grfAccessPermissions verbatim
// into the ACE. The observable result is the same either way: an ACE this package
// wrote as GENERIC_ALL reads back carrying these specific bits, which is what lets
// daclMatches recognize its own work. Stated precisely because that read-back is
// the single assumption daclMatches rests on, and it has never been confirmed by
// running on Windows: CI is ubuntu-only, so this package is type-checked under
// GOOS=windows but never executed there.
const fileFullControl = windows.ACCESS_MASK(windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1FF)

// hardenDir creates dir if missing and makes sure it carries the owner-only
// DACL either way -- a directory created by an older scrim version (or by
// hand) keeps whatever permissive ACL it inherited until this rewrites it.
//
// The ACEs carry container+object inheritance so everything scrim creates
// under the directory later (canvases/, meta/, versions/, the state file, the
// log file) comes up owner-only too, instead of inheriting the profile's
// looser ACL.
func hardenDir(dir string) error {
	// The mode is not a Windows concept and os.Mkdir discards it outright;
	// it's passed for call-shape parity with the Unix implementation. The
	// owner-only guarantee comes entirely from the DACL applied below.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating dir %s: %w", dir, err)
	}
	err := ensureOwnerOnlyDACL(dir, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
	switch {
	case err == nil:
		return nil
	case isACLUnsupported(err):
		warnACLsUnsupported()
		return nil
	default:
		return err
	}
}

// hardenFile makes sure an existing file at path carries the owner-only DACL,
// with non-inheritable ACEs (a file has nothing to propagate to). A missing
// file is not an error -- there's nothing to tighten yet, and the directory
// ACL's inheritance covers whatever creates it later.
func hardenFile(path string) error {
	err := ensureOwnerOnlyDACL(path, windows.NO_INHERITANCE)
	switch {
	case err == nil, isNotExist(err):
		return nil
	case isACLUnsupported(err):
		warnACLsUnsupported()
		return nil
	default:
		return err
	}
}

// ensureOwnerOnlyDACL makes path's DACL grant full control to exactly the
// trustees ownerOnlyTrustees returns and to nobody else, protected so the
// parent's (permissive) inheritable ACEs cannot leak back in.
//
// The write is conditional: a DACL that already matches is left alone. That
// matters much more here than the equivalent check would on Unix.
// SetNamedSecurityInfo on a container re-propagates inheritable ACEs to every
// existing descendant, and HardenPermissions runs from daemon.Ensure on every
// verb that touches the daemon -- so an unconditional write would turn `scrim
// list` into a recursive re-ACL of canvases/ and versions/, unbounded in the
// size of the user's data, where Unix does one O(1) chmod. It also means a
// mistyped --dir is read, found wanting, and only then rewritten, rather than
// rewritten reflexively.
//
// inheritance is the EXPLICIT_ACCESS inheritance flag applied to every ACE:
// SUB_CONTAINERS_AND_OBJECTS_INHERIT for a directory, NO_INHERITANCE for a
// file.
func ensureOwnerOnlyDACL(path string, inheritance uint32) error {
	// One read serves both purposes: the owner feeds the trustee set, and the
	// DACL feeds the already-correct check.
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("reading the security descriptor of %s: %w", path, err)
	}

	want, err := ownerOnlyTrustees(sd)
	if err != nil {
		return err
	}
	if daclMatches(sd, want, inheritance) {
		return nil
	}

	entries := make([]windows.EXPLICIT_ACCESS, 0, len(want))
	for _, sid := range want {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}

	// The second argument is the ACL to merge with: nil means "build this ACL
	// from the explicit entries alone", i.e. drop whatever the object carried
	// before. Merging would defeat the whole point.
	//
	// windows.TrusteeValue is a uintptr, so entries does not keep the SIDs it
	// points at reachable for the GC -- hold them until ACLFromEntries, which
	// copies them into the returned ACL, has run.
	acl, err := windows.ACLFromEntries(entries, nil)
	runtime.KeepAlive(want)
	if err != nil {
		return fmt.Errorf("building an owner-only ACL for %s: %w", path, err)
	}

	// PROTECTED_DACL_SECURITY_INFORMATION is the inheritance-disable: without
	// it the parent directory's inheritable ACEs are re-applied on top of
	// ours. Owner and group are passed as nil deliberately -- reassigning
	// ownership can require a privilege the daemon doesn't hold, and we don't
	// need to: the current owner is already one of the granted trustees.
	err = windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
	if err != nil {
		return fmt.Errorf("applying an owner-only ACL to %s: %w", path, err)
	}
	return nil
}

// daclMatches reports whether sd already carries exactly the DACL
// ensureOwnerOnlyDACL would write: protected, and one full-control allow ACE
// per want trustee with the expected inheritance flags and nothing else. Any
// doubt (an unreadable field, an ACE it can't account for) answers false --
// the fallback is doing the write, which is always correct, just slower.
func daclMatches(sd *windows.SECURITY_DESCRIPTOR, want []*windows.SID, inheritance uint32) bool {
	if sd == nil || len(want) == 0 {
		return false
	}
	control, _, err := sd.Control()
	if err != nil {
		return false
	}
	if control&windows.SE_DACL_PRESENT == 0 || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || int(dacl.AceCount) != len(want) {
		return false
	}

	// winnt.h gives the EXPLICIT_ACCESS Inheritance field and the ACE header's
	// AceFlags the same encoding for the inheritance bits (OBJECT_INHERIT_ACE
	// == SUB_OBJECTS_ONLY_INHERIT == 0x1, and so on), which is why they share
	// the VALID_INHERIT_FLAGS mask -- so the flag we asked for is the flag we
	// expect to read back. INHERITED_ACE is inside that mask too, so an ACE
	// that arrived by inheritance rather than by us won't match.
	wantFlags := inheritance & windows.VALID_INHERIT_FLAGS

	// Each want trustee must be named by exactly one ACE. Counting coverage
	// rather than mere membership is what rejects a DACL of three identical
	// SYSTEM ACEs against a three-trustee want set.
	covered := make([]bool, len(want))
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return false
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false
		}
		if uint32(ace.Header.AceFlags)&windows.VALID_INHERIT_FLAGS != wantFlags {
			return false
		}
		if ace.Mask&fileFullControl != fileFullControl {
			return false
		}
		idx := indexOfSID(want, aceSID(ace))
		if idx < 0 || covered[idx] {
			return false
		}
		covered[idx] = true
	}
	return true
}

// ownerOnlyTrustees is the set of SIDs that keep full control of the object sd
// describes, deduplicated by SID equality (the object's owner is very often
// also the process user). A nil sd -- which windows.GetNamedSecurityInfo
// documents as possible for an object that carries no security descriptor at
// all -- simply contributes no owner.
//
// SYSTEM and BUILTIN\Administrators are included on purpose. That's the
// per-user-profile convention on Windows -- %USERPROFILE% itself is exactly
// Owner + SYSTEM + Administrators -- and excluding Administrators would buy
// nothing: an administrator can always take ownership and rewrite the DACL.
// It would only break backup, anti-malware, and administrative tooling.
func ownerOnlyTrustees(sd *windows.SECURITY_DESCRIPTOR) ([]*windows.SID, error) {
	var owner *windows.SID
	if sd != nil {
		var err error
		owner, _, err = sd.Owner()
		if err != nil {
			return nil, fmt.Errorf("reading the object owner: %w", err)
		}
	}
	// The process token user is granted even when it differs from the owner,
	// so that once a DACL is written the account scrim runs as is guaranteed
	// to be on it -- an owner-only grant would otherwise leave the daemon
	// locked out of a directory owned by someone else (BUILTIN\Administrators,
	// say, for a dir first created from an elevated shell). It is not a
	// recovery mechanism: writing the DACL at all needs WRITE_DAC on the
	// object, so an account that can't already modify the object fails at
	// SetNamedSecurityInfo before any grant is written.
	user, err := currentProcessUser()
	if err != nil {
		return nil, err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, fmt.Errorf("resolving the SYSTEM SID: %w", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, fmt.Errorf("resolving the Administrators SID: %w", err)
	}
	return dedupeSIDs(owner, user, system, admins), nil
}

// currentProcessUser is the user SID of the token the daemon is running
// under.
func currentProcessUser() (*windows.SID, error) {
	// OpenProcessToken rather than the deprecated OpenCurrentProcessToken --
	// same thing, minus the deprecation, and TOKEN_QUERY is all a token-user
	// read needs. It's a real handle, so it has to be closed.
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return nil, fmt.Errorf("opening the current process token: %w", err)
	}
	defer func() { _ = token.Close() }()

	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("reading the current process token user: %w", err)
	}
	return user.User.Sid, nil
}

// dedupeSIDs drops nils and repeats (by SID equality, not pointer identity)
// while preserving order. A duplicated trustee is not an error for
// SetEntriesInAcl, but it produces a redundant ACE.
func dedupeSIDs(sids ...*windows.SID) []*windows.SID {
	out := make([]*windows.SID, 0, len(sids))
	for _, sid := range sids {
		if sid == nil || indexOfSID(out, sid) >= 0 {
			continue
		}
		out = append(out, sid)
	}
	return out
}

// aceSID returns the trustee an allow ACE names. An ACE is a variable-length
// structure whose SID begins at the SidStart field, so pointer arithmetic is
// the only way to reach it -- this is how x/sys/windows's own tests read one
// (syscall_windows_test.go:506). It is safe here: the ACE is owned by a live
// Go-heap DACL, SidStart is the documented offset of that ACE's SID, and no
// arithmetic beyond &field is done.
//
//nolint:gosec // G103: unsafe.Pointer on a live, Go-owned ACE -- see above.
func aceSID(ace *windows.ACCESS_ALLOWED_ACE) *windows.SID {
	return (*windows.SID)(unsafe.Pointer(&ace.SidStart))
}

// indexOfSID returns the position of want in sids by SID equality, or -1.
func indexOfSID(sids []*windows.SID, want *windows.SID) int {
	for i, sid := range sids {
		if sid.Equals(want) {
			return i
		}
	}
	return -1
}

// isNotExist reports whether err is the Windows "there is no such file or
// directory" family, matching the os.ErrNotExist tolerance the Unix
// implementation gets from os.Chmod.
//
// os.ErrNotExist alone is the whole check on purpose: syscall.Errno.Is maps
// ERROR_FILE_NOT_FOUND, ERROR_PATH_NOT_FOUND *and* ERROR_BAD_NETPATH onto it
// ($GOROOT/src/syscall/syscall_windows.go:202-206), and that third one -- a
// UNC --dir whose server or share doesn't resolve -- is exactly the case an
// explicit two-errno list would miss.
func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

// isACLUnsupported reports whether err means the filesystem holding the object
// has no concept of an ACL to set: FAT32/exFAT removable media, or a network
// share that exposes no per-object security. syscall.Errno.Is maps
// ERROR_NOT_SUPPORTED and ERROR_CALL_NOT_IMPLEMENTED onto errors.ErrUnsupported
// ($GOROOT/src/syscall/syscall_windows.go:207-213).
//
// ERROR_INVALID_FUNCTION is matched on top of that set: it is what a volume
// whose driver implements no security IRP at all reports, and it is the errno
// most commonly observed from the GetNamedSecurityInfo/SetNamedSecurityInfo
// family on FAT-formatted media -- the exact case this function exists for.
// Go does not fold it into errors.ErrUnsupported, so it has to be named. It
// cannot mask a real failure: a permission problem, a bad path, or a
// malformed ACL each report their own errno.
//
// This case is tolerated (one-time warning, startup continues) because there
// is genuinely nothing to enforce -- refusing to start would make a
// FAT-formatted --dir unusable rather than merely unprotected.
//
// ERROR_ACCESS_DENIED is deliberately NOT tolerated here and stays fatal. That
// is the symmetric choice: on Unix an EPERM from os.Chmod aborts startup too.
// A permission failure means the filesystem does support the restriction and
// we simply failed to apply it, so the daemon would be coming up serving data
// it has told the user is owner-only while it is not.
func isACLUnsupported(err error) bool {
	return errors.Is(err, errors.ErrUnsupported) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION)
}

var aclUnsupportedWarnOnce sync.Once

// warnACLsUnsupported logs, once per process, that the filesystem under --dir
// can't hold an ACL. HardenPermissions runs on every self-start check, not
// once per daemon lifetime, so without the guard it would spam.
func warnACLsUnsupported() {
	aclUnsupportedWarnOnce.Do(func() {
		logging.Error(logging.CategoryConfig, errACLsUnsupported)
	})
}

// errACLsUnsupported is a static, pre-scrubbed message -- it carries no path
// or other caller-derived text, matching what internal/logging accepts.
var errACLsUnsupported = errors.New(
	"owner-only permission hardening is unavailable: the filesystem holding " +
		"the scrim data directory does not support access-control lists, so " +
		"--dir, the state file, and the log file are not being tightened",
)
