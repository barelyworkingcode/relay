#import "localauth_darwin.h"
#import <LocalAuthentication/LocalAuthentication.h>

extern void relayPresenceLAResult(uintptr_t token, int success, long long code);

// Pending evaluations by token, so relay_la_invalidate can reach the context
// an evaluation is still waiting on. This file is compiled without ARC: the
// dictionary owns each context, and removing the entry releases it.
static NSMutableDictionary<NSNumber *, LAContext *> *relayLAContexts(void) {
	static NSMutableDictionary *contexts;
	static dispatch_once_t once;
	dispatch_once(&once, ^{
		contexts = [[NSMutableDictionary alloc] init];
	});
	return contexts;
}

void relay_la_evaluate(const char *reason, uintptr_t token) {
	@autoreleasepool {
		NSString *nsReason = [NSString stringWithUTF8String:reason];
		NSNumber *key = [NSNumber numberWithUnsignedLongLong:(unsigned long long)token];
		NSMutableDictionary *contexts = relayLAContexts();

		// A fresh context per evaluation, left at the default (zero)
		// touchIDAuthenticationAllowableReuseDuration: an allowable reuse
		// window is an ambient session, which the presence model forbids
		// (§3.1 of the ADR-017 implementation spec).
		LAContext *context = [[LAContext alloc] init];
		@synchronized (contexts) {
			[contexts setObject:context forKey:key];
		}
		[context release];

		[context evaluatePolicy:LAPolicyDeviceOwnerAuthentication
				 localizedReason:nsReason
						   reply:^(BOOL success, NSError *error) {
			long long code = 0;
			if (error != nil) {
				code = (long long)error.code;
			}
			relayPresenceLAResult(token, success ? 1 : 0, code);
			@synchronized (contexts) {
				[contexts removeObjectForKey:key];
			}
		}];
	}
}

void relay_la_invalidate(uintptr_t token) {
	@autoreleasepool {
		NSNumber *key = [NSNumber numberWithUnsignedLongLong:(unsigned long long)token];
		NSMutableDictionary *contexts = relayLAContexts();
		LAContext *context = nil;
		@synchronized (contexts) {
			context = [[contexts objectForKey:key] retain];
		}
		[context invalidate];
		[context release];
	}
}
