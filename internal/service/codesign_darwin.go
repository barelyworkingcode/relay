//go:build darwin

package service

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation

#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>

// relayCheckStaticRequirement validates the on-disk code at path against a
// requirement string (SP3: "anchor apple generic and certificate
// leaf[subject.OU]=..." for a team pin, "cdhash H\"...\"" for a cdhash
// pin). Returns an OSStatus; errSecSuccess (0) is the only passing value.
static OSStatus relayCheckStaticRequirement(const char *path, const char *requirementStr) {
    CFStringRef pathStr = CFStringCreateWithCString(NULL, path, kCFStringEncodingUTF8);
    if (pathStr == NULL) {
        return errSecParam;
    }
    CFURLRef url = CFURLCreateWithFileSystemPath(NULL, pathStr, kCFURLPOSIXPathStyle, false);
    CFRelease(pathStr);
    if (url == NULL) {
        return errSecParam;
    }

    SecStaticCodeRef code = NULL;
    OSStatus status = SecStaticCodeCreateWithPath(url, kSecCSDefaultFlags, &code);
    CFRelease(url);
    if (status != errSecSuccess) {
        return status;
    }

    CFStringRef reqStr = CFStringCreateWithCString(NULL, requirementStr, kCFStringEncodingUTF8);
    SecRequirementRef req = NULL;
    status = SecRequirementCreateWithString(reqStr, kSecCSDefaultFlags, &req);
    CFRelease(reqStr);
    if (status != errSecSuccess) {
        CFRelease(code);
        return status;
    }

    status = SecStaticCodeCheckValidity(code, kSecCSDefaultFlags, req);
    CFRelease(req);
    CFRelease(code);
    return status;
}

// relayCheckGuestRequirement resolves the live process named by a 32-byte
// audit_token_t (SOL_LOCAL/LOCAL_PEERTOKEN, read Go-side over the accepted
// connection) to a SecCodeRef via kSecGuestAttributeAudit, then validates
// it against requirementStr exactly as relayCheckStaticRequirement does for
// a file on disk. Verified (SP3) to need no extra entitlement from an
// unsandboxed, hardened-runtime process.
static OSStatus relayCheckGuestRequirement(const unsigned char *auditToken, CFIndex tokenLen, const char *requirementStr) {
    CFDataRef tokenData = CFDataCreate(NULL, auditToken, tokenLen);
    if (tokenData == NULL) {
        return errSecParam;
    }
    CFTypeRef keys[1] = { kSecGuestAttributeAudit };
    CFTypeRef values[1] = { tokenData };
    CFDictionaryRef attrs = CFDictionaryCreate(NULL, (const void **)keys, (const void **)values, 1,
        &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    CFRelease(tokenData);
    if (attrs == NULL) {
        return errSecParam;
    }

    SecCodeRef guest = NULL;
    OSStatus status = SecCodeCopyGuestWithAttributes(NULL, attrs, kSecCSDefaultFlags, &guest);
    CFRelease(attrs);
    if (status != errSecSuccess) {
        return status;
    }

    CFStringRef reqStr = CFStringCreateWithCString(NULL, requirementStr, kCFStringEncodingUTF8);
    SecRequirementRef req = NULL;
    status = SecRequirementCreateWithString(reqStr, kSecCSDefaultFlags, &req);
    CFRelease(reqStr);
    if (status != errSecSuccess) {
        CFRelease(guest);
        return status;
    }

    status = SecCodeCheckValidity(guest, kSecCSDefaultFlags, req);
    CFRelease(req);
    CFRelease(guest);
    return status;
}
*/
import "C"

import (
	"fmt"
	"unsafe"

	"github.com/barelyworkingcode/relay/internal/peertoken"
)

// darwinHelperVerifier is the real, cgo-backed HelperVerifier (SP3). team
// is a Developer-ID team requirement string's OU value; empty skips the
// team check entirely — SP3's ad-hoc note: it cannot be satisfied without a
// certificate, so an ad-hoc build gates on cdhash alone. cdhash is always
// required and is what actually proves exact-binary identity (SP3's swap
// test: a team pin alone accepts a same-team wrong binary).
type darwinHelperVerifier struct {
	team   string
	cdhash string
}

// NewDarwinHelperVerifier builds the production HelperVerifier from the
// build-time values build.sh embeds via -ldflags -X: team is relay's own
// Developer ID team OU (empty for an ad-hoc build), cdhash is
// Contents/Helpers/relay-sessions' own CDHash, read and embedded in the
// exact order SP3 verified (sign helper -> read its cdhash -> embed ->
// build+sign relay). An empty cdhash is a construction error: nothing this
// type checks can pass without one.
func NewDarwinHelperVerifier(team, cdhash string) (HelperVerifier, error) {
	if cdhash == "" {
		return nil, fmt.Errorf("service: NewDarwinHelperVerifier: no helper cdhash embedded in this build")
	}
	return &darwinHelperVerifier{team: team, cdhash: cdhash}, nil
}

func (v *darwinHelperVerifier) cdhashRequirement() string {
	return fmt.Sprintf(`cdhash H"%s"`, v.cdhash)
}

func (v *darwinHelperVerifier) teamRequirement() string {
	return fmt.Sprintf(`anchor apple generic and certificate leaf[subject.OU] = "%s"`, v.team)
}

// VerifyStatic checks the on-disk binary at path: the cheap team-cert
// prefilter first (skipped entirely for an ad-hoc build, per SP3), then the
// cdhash pin that is the actual gate. Either failing refuses.
func (v *darwinHelperVerifier) VerifyStatic(path string) error {
	if v.team != "" {
		if status := checkStatic(path, v.teamRequirement()); status != C.errSecSuccess {
			return fmt.Errorf("service: helper %s failed its team-identity check: OSStatus %d", path, int(status))
		}
	}
	if status := checkStatic(path, v.cdhashRequirement()); status != C.errSecSuccess {
		return fmt.Errorf("service: helper %s failed its cdhash check: OSStatus %d", path, int(status))
	}
	return nil
}

// VerifyGuest checks the live process peer names, by its kernel-attested
// audit token, against the same two requirements VerifyStatic used for the
// file on disk (SP3's runtime half — the check that actually stops a
// process that merely guessed or stole the launch secret).
func (v *darwinHelperVerifier) VerifyGuest(peer peertoken.Token) error {
	if !peer.Valid() {
		return fmt.Errorf("service: no peer audit token to verify the helper's guest code identity")
	}
	if v.team != "" {
		if status := checkGuest(peer, v.teamRequirement()); status != C.errSecSuccess {
			return fmt.Errorf("service: helper guest failed its team-identity check: OSStatus %d", int(status))
		}
	}
	if status := checkGuest(peer, v.cdhashRequirement()); status != C.errSecSuccess {
		return fmt.Errorf("service: helper guest failed its cdhash check: OSStatus %d", int(status))
	}
	return nil
}

func checkStatic(path, requirement string) C.OSStatus {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	cReq := C.CString(requirement)
	defer C.free(unsafe.Pointer(cReq))
	return C.relayCheckStaticRequirement(cPath, cReq)
}

func checkGuest(peer peertoken.Token, requirement string) C.OSStatus {
	raw := peer.Raw()
	cReq := C.CString(requirement)
	defer C.free(unsafe.Pointer(cReq))
	return C.relayCheckGuestRequirement((*C.uchar)(unsafe.Pointer(&raw[0])), C.CFIndex(len(raw)), cReq)
}
