module github.com/barelyworkingcode/relay

go 1.25.0

require (
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
)

require (
	github.com/dop251/goja v0.0.0-20260607120635-348e6bea910d
	github.com/evanw/esbuild v0.28.0
	github.com/fxamacker/cbor/v2 v2.9.3
	github.com/hugelgupf/p9 v0.4.1
	github.com/modelcontextprotocol/go-sdk v1.5.0
	github.com/tidwall/jsonc v0.3.3
	golang.org/x/sys v0.41.0
)

require (
	// Pinned ahead of use: R-S6 imports this package directly for terminal/pty
	// handling. Only R-S7a may edit go.mod during the parallel session-host
	// units, so the version is staged here now (SP4-verified clean) rather
	// than left for R-S6 to bump later.
	github.com/creack/pty v1.1.24 // indirect
	github.com/dlclark/regexp2/v2 v2.2.1 // indirect
	github.com/go-sourcemap/sourcemap v2.1.3+incompatible // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/google/pprof v0.0.0-20230207041349-798e818bf904 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/u-root/uio v0.0.0-20230305220412-3e8cd9d6bf63 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/text v0.3.8 // indirect
)
