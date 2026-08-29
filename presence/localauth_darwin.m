#import "localauth_darwin.h"
#import <LocalAuthentication/LocalAuthentication.h>

extern void relayPresenceLAResult(uintptr_t token, int success, long long code);

void relay_la_evaluate(const char *reason, uintptr_t token) {
	@autoreleasepool {
		NSString *nsReason = [NSString stringWithUTF8String:reason];

		// A fresh context per evaluation, left at the default (zero)
		// touchIDAuthenticationAllowableReuseDuration: an allowable reuse
		// window is an ambient session, which the presence model forbids
		// (§3.1 of the ADR-017 implementation spec).
		LAContext *context = [[LAContext alloc] init];

		[context evaluatePolicy:LAPolicyDeviceOwnerAuthentication
				 localizedReason:nsReason
						   reply:^(BOOL success, NSError *error) {
			long long code = 0;
			if (error != nil) {
				code = (long long)error.code;
			}
			relayPresenceLAResult(token, success ? 1 : 0, code);
		}];
	}
}
