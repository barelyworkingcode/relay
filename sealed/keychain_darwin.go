//go:build darwin

package sealed

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation

#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>

// SecTrustedApplicationCreateFromPath and SecAccessCreate are deprecated
// since macOS 10.10. This is a deliberate deprecated-API bet, not an
// oversight: kSecAttrAccessControl is not usable here (every SecItemAdd
// carrying it measured -34018 errSecMissingEntitlement, and the
// entitlement needed to get past that requires a portal-issued
// provisioning profile relay does not have). The durable reasoning lives in
// docs/sealed-config.md. The pragma is scoped to this file so a deprecated
// call added anywhere else in the module still warns.
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"

// relaySealAccess builds a SecAccess granting decrypt-without-prompt to the
// code identity at appPath and nothing else. SecTrustedApplicationCreateFromPath
// reads only the code identity at appPath, at call time -- the resulting ACL
// binds to that identity (team + bundle id), never to the path or to a
// cdhash, and survives a rebuild signed by the same identity (§5.3.2).
static SecAccessRef relaySealAccess(const char *appPath, OSStatus *status) {
    SecTrustedApplicationRef app = NULL;
    *status = SecTrustedApplicationCreateFromPath(appPath, &app);
    if (*status != errSecSuccess || app == NULL) {
        return NULL;
    }

    CFArrayRef trustedList = CFArrayCreate(NULL, (const void **)&app, 1, &kCFTypeArrayCallBacks);
    CFRelease(app);

    SecAccessRef access = NULL;
    *status = SecAccessCreate(CFSTR("relay config seal key"), trustedList, &access);
    CFRelease(trustedList);
    if (*status != errSecSuccess) {
        return NULL;
    }
    return access;
}

// relaySealAdd stores a generic-password item under service/account, with
// its value set to the dataLen bytes at data and its ACL set to access.
static OSStatus relaySealAdd(const char *service, const char *account,
                              const void *data, CFIndex dataLen,
                              SecAccessRef access) {
    CFStringRef serviceStr = CFStringCreateWithCString(NULL, service, kCFStringEncodingUTF8);
    CFStringRef accountStr = CFStringCreateWithCString(NULL, account, kCFStringEncodingUTF8);
    CFDataRef dataRef = CFDataCreate(NULL, (const UInt8 *)data, dataLen);

    CFTypeRef keys[6];
    CFTypeRef values[6];
    CFIndex n = 0;
    keys[n] = kSecClass;          values[n] = kSecClassGenericPassword; n++;
    keys[n] = kSecAttrService;    values[n] = serviceStr; n++;
    keys[n] = kSecAttrAccount;    values[n] = accountStr; n++;
    keys[n] = kSecValueData;      values[n] = dataRef; n++;
    keys[n] = kSecAttrAccessible; values[n] = kSecAttrAccessibleWhenUnlockedThisDeviceOnly; n++;
    if (access != NULL) {
        keys[n] = kSecAttrAccess; values[n] = access; n++;
    }

    CFDictionaryRef query = CFDictionaryCreate(NULL, (const void **)keys, (const void **)values, n,
        &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);

    OSStatus status = SecItemAdd(query, NULL);

    CFRelease(query);
    CFRelease(dataRef);
    CFRelease(accountStr);
    CFRelease(serviceStr);
    return status;
}

// relaySealCopy reads the generic-password item's value. On errSecSuccess
// the caller owns *outData and must CFRelease it.
static OSStatus relaySealCopy(const char *service, const char *account, CFDataRef *outData) {
    CFStringRef serviceStr = CFStringCreateWithCString(NULL, service, kCFStringEncodingUTF8);
    CFStringRef accountStr = CFStringCreateWithCString(NULL, account, kCFStringEncodingUTF8);

    CFTypeRef keys[5];
    CFTypeRef values[5];
    CFIndex n = 0;
    keys[n] = kSecClass;       values[n] = kSecClassGenericPassword; n++;
    keys[n] = kSecAttrService; values[n] = serviceStr; n++;
    keys[n] = kSecAttrAccount; values[n] = accountStr; n++;
    keys[n] = kSecReturnData;  values[n] = kCFBooleanTrue; n++;
    keys[n] = kSecMatchLimit;  values[n] = kSecMatchLimitOne; n++;

    CFDictionaryRef query = CFDictionaryCreate(NULL, (const void **)keys, (const void **)values, n,
        &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);

    CFTypeRef result = NULL;
    OSStatus status = SecItemCopyMatching(query, &result);

    CFRelease(query);
    CFRelease(accountStr);
    CFRelease(serviceStr);

    if (status != errSecSuccess) {
        return status;
    }
    *outData = (CFDataRef)result;
    return errSecSuccess;
}

// relaySealDelete removes the generic-password item, if one exists.
static OSStatus relaySealDelete(const char *service, const char *account) {
    CFStringRef serviceStr = CFStringCreateWithCString(NULL, service, kCFStringEncodingUTF8);
    CFStringRef accountStr = CFStringCreateWithCString(NULL, account, kCFStringEncodingUTF8);

    CFTypeRef keys[3] = { kSecClass, kSecAttrService, kSecAttrAccount };
    CFTypeRef values[3] = { kSecClassGenericPassword, serviceStr, accountStr };

    CFDictionaryRef query = CFDictionaryCreate(NULL, (const void **)keys, (const void **)values, 3,
        &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);

    OSStatus status = SecItemDelete(query);

    CFRelease(query);
    CFRelease(accountStr);
    CFRelease(serviceStr);
    return status;
}

#pragma clang diagnostic pop
*/
import "C"

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"
)

const (
	keychainService = "com.barelyworkingcode.relay"
	keychainAccount = "config-seal-key"
)

// keychainPayload is the item's value: the id travels with the key so it
// cannot drift from it (§5.2).
type keychainPayload struct {
	KeyID string `json:"key_id"`
	Key   string `json:"key"`
}

func encodeKeychainPayload(keyID string, key []byte) ([]byte, error) {
	b, err := json.Marshal(keychainPayload{KeyID: keyID, Key: base64.StdEncoding.EncodeToString(key)})
	if err != nil {
		return nil, fmt.Errorf("sealed: encoding keychain payload: %w", err)
	}
	return b, nil
}

func decodeKeychainPayload(data []byte) (keyID string, key []byte, err error) {
	var p keychainPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return "", nil, fmt.Errorf("sealed: keychain item is not the expected payload: %w", err)
	}
	key, err = base64.StdEncoding.DecodeString(p.Key)
	if err != nil {
		return "", nil, fmt.Errorf("sealed: keychain item's key is not valid base64: %w", err)
	}
	return p.KeyID, key, nil
}

// keychainKeyring is the login-keychain Keyring: item class
// kSecClassGenericPassword, service/account below, accessibility
// kSecAttrAccessibleWhenUnlockedThisDeviceOnly (never syncs to iCloud,
// never restores onto another machine), ACL'd to trustedAppPath's code
// identity (§5.3).
type keychainKeyring struct {
	trustedAppPath string
	service        string
	account        string
}

// NewKeychainKeyring returns a Keyring backed by the login keychain, whose
// item only the code identity at trustedAppPath may read without a
// consent prompt (§5.3.1). Construct this at exactly one call site in
// production -- the tray's own path, from runTrayApp -- and never from a
// CLI entry point: the CLI carries relay's own code identity too, so a
// second call site here would satisfy the ACL by being exactly the process
// §5.3.3 defends against (AC-29).
func NewKeychainKeyring(trustedAppPath string) Keyring {
	return &keychainKeyring{
		trustedAppPath: trustedAppPath,
		service:        keychainService,
		account:        keychainAccount,
	}
}

func (k *keychainKeyring) Load() (string, []byte, error) {
	data, err := k.copyItem()
	if err != nil {
		return "", nil, err
	}
	return decodeKeychainPayload(data)
}

func (k *keychainKeyring) Create() (string, []byte, error) {
	if _, _, err := k.Load(); err == nil {
		return "", nil, fmt.Errorf("sealed: a key already exists under %s/%s; refusing to overwrite", k.service, k.account)
	} else if !errors.Is(err, ErrKeyMissing) {
		return "", nil, err
	}

	keyID, key, err := generateKey()
	if err != nil {
		return "", nil, err
	}
	payload, err := encodeKeychainPayload(keyID, key)
	if err != nil {
		return "", nil, err
	}
	if err := k.addItem(payload); err != nil {
		return "", nil, err
	}
	return keyID, key, nil
}

func (k *keychainKeyring) Destroy() error {
	return k.deleteItem()
}

func (k *keychainKeyring) addItem(payload []byte) error {
	cAppPath := C.CString(k.trustedAppPath)
	defer C.free(unsafe.Pointer(cAppPath))

	var status C.OSStatus
	access := C.relaySealAccess(cAppPath, &status)
	if status != C.errSecSuccess {
		return fmt.Errorf("sealed: building keychain ACL for %q: OSStatus %d", k.trustedAppPath, int(status))
	}
	defer C.CFRelease(C.CFTypeRef(access))

	cService := C.CString(k.service)
	defer C.free(unsafe.Pointer(cService))
	cAccount := C.CString(k.account)
	defer C.free(unsafe.Pointer(cAccount))

	var dataPtr unsafe.Pointer
	if len(payload) > 0 {
		dataPtr = C.CBytes(payload)
		defer C.free(dataPtr)
	}

	status = C.relaySealAdd(cService, cAccount, dataPtr, C.CFIndex(len(payload)), access)
	switch status {
	case C.errSecSuccess:
		return nil
	case C.errSecDuplicateItem:
		return fmt.Errorf("sealed: a key already exists under %s/%s; refusing to overwrite", k.service, k.account)
	default:
		return fmt.Errorf("sealed: adding keychain item: OSStatus %d", int(status))
	}
}

func (k *keychainKeyring) copyItem() ([]byte, error) {
	cService := C.CString(k.service)
	defer C.free(unsafe.Pointer(cService))
	cAccount := C.CString(k.account)
	defer C.free(unsafe.Pointer(cAccount))

	var out C.CFDataRef
	status := C.relaySealCopy(cService, cAccount, &out)
	switch status {
	case C.errSecSuccess:
	case C.errSecItemNotFound:
		return nil, ErrKeyMissing
	default:
		return nil, fmt.Errorf("sealed: reading keychain item: OSStatus %d", int(status))
	}
	defer C.CFRelease(C.CFTypeRef(out))

	n := C.CFDataGetLength(out)
	ptr := C.CFDataGetBytePtr(out)
	return C.GoBytes(unsafe.Pointer(ptr), C.int(n)), nil
}

func (k *keychainKeyring) deleteItem() error {
	cService := C.CString(k.service)
	defer C.free(unsafe.Pointer(cService))
	cAccount := C.CString(k.account)
	defer C.free(unsafe.Pointer(cAccount))

	status := C.relaySealDelete(cService, cAccount)
	if status != C.errSecSuccess && status != C.errSecItemNotFound {
		return fmt.Errorf("sealed: deleting keychain item: OSStatus %d", int(status))
	}
	return nil
}
