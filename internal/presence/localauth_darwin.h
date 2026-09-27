#ifndef PRESENCE_LOCALAUTH_DARWIN_H
#define PRESENCE_LOCALAUTH_DARWIN_H

#include <stdint.h>

// relay_la_evaluate asks LocalAuthentication to evaluate
// LAPolicyDeviceOwnerAuthentication with reason. evaluatePolicy is
// asynchronous; the result is reported later to the Go side via the
// relayPresenceLAResult cgo export, keyed by token.
void relay_la_evaluate(const char *reason, uintptr_t token);

// relay_la_invalidate invalidates the context of the evaluation started with
// token, if it is still pending: the dialog is dismissed and the reply fails
// with LAErrorAppCancel. A token whose reply has already arrived is a no-op.
void relay_la_invalidate(uintptr_t token);

#endif
