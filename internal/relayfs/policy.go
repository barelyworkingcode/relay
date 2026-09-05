package relayfs

import "github.com/hugelgupf/p9/p9"

// maskCreateMode strips S_ISUID, S_ISGID and S_ISVTX from a mode a client
// asked to create something with (create, mkdir, chmod all funnel through
// this) — normalised, not refused, so `tar -xp`/`rsync -a` on an ordinary
// archive succeed; the bits do nothing useful here anyway (the mount is
// nosuid on the client, and this server never executes anything).
func maskCreateMode(mode p9.FileMode) p9.FileMode {
	const isuidIsgidIsvtx = p9.FileMode(04000 | 02000 | 01000)
	return mode &^ isuidIsgidIsvtx
}

// isAppleXattr reports whether name is one of the com.apple.* extended
// attributes this server must never read or write on a client's behalf —
// quarantine, FinderInfo, ResourceFork, ACLs, and anything else under the
// same prefix are host-system semantics.
func isAppleXattr(name string) bool {
	const prefix = "com.apple."
	return len(name) >= len(prefix) && name[:len(prefix)] == prefix
}
