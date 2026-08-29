//go:build darwin

package presence

import "testing"

// TestClassifyLAResult_NumericCodesOnly is the whole of AC-19g: success and
// -2 (LAErrorUserCancel) are the only two outcomes recognised, and every
// other numeric code — including the undocumented -1000 measured in a
// sessionless context, and any code Apple might add tomorrow — is failure,
// indistinguishably. This never calls the real LocalAuthentication API: it
// exercises classifyLAResult directly, so the hermetic suite never risks a
// real prompt.
func TestClassifyLAResult_NumericCodesOnly(t *testing.T) {
	cases := []struct {
		name    string
		result  laResult
		wantErr error
	}{
		{"success", laResult{success: true, code: 0}, nil},
		{"success with a stray nonzero code", laResult{success: true, code: -2}, nil},
		{"user cancel", laResult{success: false, code: laErrorUserCancel}, ErrRefused},
		{"documented NotInteractive (-1004)", laResult{success: false, code: -1004}, ErrRefused},
		{"undocumented -1000, measured in a sessionless context", laResult{success: false, code: -1000}, ErrRefused},
		{"authentication failed (-1)", laResult{success: false, code: -1}, ErrRefused},
		{"a code that does not exist yet", laResult{success: false, code: -999999}, ErrRefused},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyLAResult(c.result); got != c.wantErr {
				t.Errorf("classifyLAResult(%+v) = %v, want %v", c.result, got, c.wantErr)
			}
		})
	}
}

// TestClassifyLAResult_MinusOneThousandIsNotSpecialCased proves the second
// half of AC-19g directly: -1000's classification is identical to an
// arbitrary undocumented code's, so removing any code-specific handling for
// -1000 (there is none) could not change behaviour.
func TestClassifyLAResult_MinusOneThousandIsNotSpecialCased(t *testing.T) {
	minusOneThousand := classifyLAResult(laResult{success: false, code: -1000})
	arbitraryOtherCode := classifyLAResult(laResult{success: false, code: -424242})
	if minusOneThousand != arbitraryOtherCode {
		t.Fatalf("-1000 classified as %v but an arbitrary undocumented code classified as %v; -1000 must not be special-cased", minusOneThousand, arbitraryOtherCode)
	}
}

func TestLocalAuthProviderConstructions_StartsAtZero(t *testing.T) {
	// This only holds because nothing above this test in the presence
	// package's test binary has called NewLocalAuthProvider — which is
	// exactly AC-20's point. TestMain (presence_seam_test.go) re-checks
	// this after the whole package's tests have run, which is the assertion
	// that actually matters; this one documents the invariant locally.
	if n := LocalAuthProviderConstructions(); n != 0 {
		t.Fatalf("LocalAuthProviderConstructions() = %d before this test constructed anything", n)
	}
}
