package projectfs

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Foundation
#import <Foundation/Foundation.h>
#include <stdlib.h>

// Returns NULL on success, otherwise a malloc'd description.
static char *relayTrashItem(const char *path) {
	@autoreleasepool {
		NSURL *url = [NSURL fileURLWithPath:[NSString stringWithUTF8String:path]];
		NSError *err = nil;
		if ([[NSFileManager defaultManager] trashItemAtURL:url resultingItemURL:nil error:&err]) {
			return NULL;
		}
		const char *msg = err ? [[err localizedDescription] UTF8String] : "trash failed";
		return strdup(msg ? msg : "trash failed");
	}
}
*/
import "C"

import "unsafe"

// trashPath moves the item at an absolute path to the user's Trash through
// NSFileManager, so it needs no Finder and no Automation consent.
func trashPath(path string) error {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	msg := C.relayTrashItem(cpath)
	if msg == nil {
		return nil
	}
	defer C.free(unsafe.Pointer(msg))
	return Errf(CodeError, "could not move to Trash: "+C.GoString(msg))
}
