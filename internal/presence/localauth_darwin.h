#ifndef PRESENCE_LOCALAUTH_DARWIN_H
#define PRESENCE_LOCALAUTH_DARWIN_H

#include <stdint.h>

// relay_la_evaluate asks LocalAuthentication to evaluate
// LAPolicyDeviceOwnerAuthentication with reason. evaluatePolicy is
// asynchronous; the result is reported later to the Go side via the
// relayPresenceLAResult cgo export, keyed by token.
void relay_la_evaluate(const char *reason, uintptr_t token);

#endif
