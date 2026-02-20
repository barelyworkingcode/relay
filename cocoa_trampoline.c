// cocoa_trampoline.c -- C trampoline for dispatch callbacks.
// Separate file to avoid cgo duplicate symbol issues.

extern void goDispatchCallback(void* ctx);

void goDispatchTrampoline(void* ctx) {
    goDispatchCallback(ctx);
}
