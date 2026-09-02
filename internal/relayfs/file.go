package relayfs

import (
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"syscall"
	"time"

	"github.com/hugelgupf/p9/linux"
	"github.com/hugelgupf/p9/p9"
	"golang.org/x/sys/unix"
)

// file implements p9.File. One value exists per 9P fid the client holds.
type file struct {
	p9.DefaultWalkGetAttr // WalkGetAttr -> ENOSYS, server falls back to Walk+GetAttr

	r                *Root
	rel              string // root-relative path; "" means the mount root itself
	open             *os.File
	names            []string
	endWriteSession  func(error)
	writeSessionOpen bool
}

var _ p9.File = (*file)(nil)

// rootPath spells this file's relative path in the form os.Root's methods
// take it: the empty relative path means "the mount root itself", and os.Root
// refuses the empty string but accepts ".".
func rootPath(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

// lstatInfo Lstats this file's own node without following a final-component
// symlink, so a symlink is reported as one, never as whatever it points to.
func (f *file) lstatInfo() (os.FileInfo, error) {
	return f.r.root.Lstat(rootPath(f.rel))
}

// qidOf lstats node (already root-relative) and builds its QID.
func (f *file) qidOf(node string) (p9.QID, error) {
	info, err := f.r.root.Lstat(rootPath(node))
	if err != nil {
		return p9.QID{}, toErrno(err)
	}
	return qidFromInfo(info), nil
}

// withOpenFD runs fn against a file descriptor that already resolved to this
// node through the Root: the open handle if this file was Opened, otherwise a
// short-lived read-only open. fd-based syscalls against it add zero path
// resolution of their own — the contained, TOCTOU-free way to reach a
// syscall os.Root does not wrap.
func (f *file) withOpenFD(fn func(fd int) error) error {
	if f.open != nil {
		return fn(int(f.open.Fd()))
	}
	h, err := f.r.root.OpenFile(rootPath(f.rel), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	err = fn(int(h.Fd()))
	if cerr := h.Close(); cerr != nil && err == nil {
		// fn's own result is the answer for this request; a failed close
		// of this short-lived handle is reported only when there is no
		// more meaningful error to report.
		return toErrno(cerr)
	}
	return err
}

// Walk walks to the path components given in names. The framework calls it
// one already-validated, slash-free, non-. / non-.. / non-empty component at
// a time (an empty slice means "return a copy of self", used for fid
// cloning), so the only path building that can ever happen here is appending
// exactly one such component — path.Join is how the next hop is spelled, not
// path canonicalisation.
func (f *file) Walk(names []string) ([]p9.QID, p9.File, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return nil, nil, toErrno(err)
	}
	if len(names) == 0 {
		return nil, &file{r: f.r, rel: f.rel}, nil
	}
	if len(names) != 1 {
		// checkSafeName guarantees one component per Twalk on this
		// server; this is a defensive refusal for a documented
		// interface possibility the framework never triggers.
		return nil, nil, linux.EINVAL
	}
	next := path.Join(f.rel, names[0])
	info, err := f.r.root.Lstat(rootPath(next))
	if err != nil {
		return nil, nil, toErrno(err)
	}
	qid := qidFromInfo(info)
	return []p9.QID{qid}, &file{r: f.r, rel: next}, nil
}

// GetAttr reports this node's attributes, lstat'd so a symlink is reported
// as itself. The full set is built and trimmed to the caller's mask.
func (f *file) GetAttr(req p9.AttrMask) (p9.QID, p9.AttrMask, p9.Attr, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return p9.QID{}, req, p9.Attr{}, toErrno(err)
	}
	info, err := f.lstatInfo()
	if err != nil {
		return p9.QID{}, req, p9.Attr{}, toErrno(err)
	}
	qid := qidFromInfo(info)
	attr := attrFromInfo(info)
	return qid, req, attr.WithMask(req), nil
}

// StatFS passes the filesystem's own aggregate numbers through: disk size is
// not a secret worth a synthetic answer. Only free-space counters cross the
// wire, nothing about names or contents inside the tree, so the
// fd-based call is the contained one.
func (f *file) StatFS() (p9.FSStat, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return p9.FSStat{}, toErrno(err)
	}
	var st unix.Statfs_t
	err := f.withOpenFD(func(fd int) error {
		return unix.Fstatfs(fd, &st)
	})
	if err != nil {
		return p9.FSStat{}, toErrno(err)
	}
	return p9.FSStat{
		Type:            st.Type,
		BlockSize:       st.Bsize,
		Blocks:          st.Blocks,
		BlocksFree:      st.Bfree,
		BlocksAvailable: st.Bavail,
		Files:           st.Files,
		FilesFree:       st.Ffree,
		FSID:            uint64(uint32(st.Fsid.Val[0]))<<32 | uint64(uint32(st.Fsid.Val[1])),
		NameLength:      255,
	}, nil
}

// Readdir lists this directory's entries. Reads are paged across calls: the
// full name list is fetched (and cached) once, and each call serves the
// window [offset, offset+count). Each entry's QID and type are populated
// per-entry from an Lstat, which is what a client-side readdir-plus depends
// on; a mid-window Lstat failure returns what accumulated so far with the
// error, and io.EOF from the name list means "no more entries", not an
// error. A zero count is the client's size probe and answers with an empty
// reply; the directory's size rides on the last entry's Offset.
func (f *file) Readdir(offset uint64, count uint32) (p9.Dirents, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return nil, toErrno(err)
	}
	if f.open == nil {
		return nil, linux.EINVAL
	}
	if f.names == nil {
		names, err := f.open.Readdirnames(-1)
		if err == io.EOF {
			f.names = []string{}
		} else if err != nil {
			return nil, toErrno(err)
		} else {
			// Readdirnames makes no order guarantee once it goes through
			// an os.Root, and a directory stream a client pages through
			// must be deterministic: sort it.
			sort.Strings(names)
			f.names = names
		}
	}
	// The offset is the index of the first name to return (0 starts; entry
	// i reports Offset i+1, so a caller resumes by passing back the last
	// Offset it received) and count bounds how many entries this call
	// processes: each one costs an Lstat, so this call serves exactly the
	// window [offset, offset+count) and does no work for entries beyond
	// it. The server-side encoder's own byte budget for the response is a
	// separate, tighter backstop that may still truncate the page.
	lo := int(offset)
	if lo > len(f.names) {
		lo = len(f.names)
	}
	hi := lo + int(count)
	if hi > len(f.names) {
		hi = len(f.names)
	}
	var ents p9.Dirents
	for i := lo; i < hi; i++ {
		name := f.names[i]
		info, err := f.r.root.Lstat(rootPath(path.Join(f.rel, name)))
		if err != nil {
			return ents, toErrno(err)
		}
		qid := qidFromInfo(info)
		ents = append(ents, p9.Dirent{
			QID:    qid,
			Type:   qid.Type,
			Name:   name,
			Offset: uint64(i + 1), // 1-based; 0 means "start".
		})
	}
	return ents, nil
}

// Readlink returns the link's target verbatim — no rewriting, no validation
// of where it points. The target is disclosed as-is on purpose; it resolves
// in the client's namespace, never on this host.
func (f *file) Readlink() (string, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return "", toErrno(err)
	}
	target, err := f.r.root.Readlink(rootPath(f.rel))
	if err != nil {
		return "", toErrno(err)
	}
	return target, nil
}

// Open opens this node. A write-flavoured open on a read-only mount is
// refused, and so is any open landing on a symlink: the client kernel always
// resolves links before opening, so a server-side open reaching one means
// the tree changed underneath the client, and os.Root's in-root following
// must not apply there.
func (f *file) Open(mode p9.OpenFlags) (p9.QID, uint32, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return p9.QID{}, 0, toErrno(err)
	}
	if !f.r.policy.Write && mode.Mode() != p9.ReadOnly {
		return p9.QID{}, 0, linux.EROFS
	}
	info, err := f.lstatInfo()
	if err != nil {
		return p9.QID{}, 0, toErrno(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		f.r.hooks.Refused("open", map[string]any{"path": f.rel}, "symlink-on-open")
		return p9.QID{}, 0, linux.ELOOP
	}
	osFile, err := f.r.root.OpenFile(rootPath(f.rel), mode.OSFlags(), 0)
	if err != nil {
		return p9.QID{}, 0, toErrno(err)
	}
	f.open = osFile
	f.writeSessionOpen = false
	f.endWriteSession = nil
	return qidFromInfo(info), 0, nil
}

// ReadAt reads from this file and charges the hook with the bytes actually
// read before they leave: a hard ceiling, not one call's worth over. io.EOF
// is preserved as-is alongside the charged count, per the interface's own
// contract.
func (f *file) ReadAt(p []byte, offset int64) (int, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return 0, toErrno(err)
	}
	if f.open == nil {
		return 0, linux.EINVAL
	}
	n, err := f.open.ReadAt(p, offset)
	if n > 0 {
		if cerr := f.r.hooks.ChargeRead(n); cerr != nil {
			return 0, linux.EDQUOT
		}
	}
	if err != nil {
		if err == io.EOF {
			return n, io.EOF
		}
		return n, toErrno(err)
	}
	return n, nil
}

// WriteAt is the write-session boundary: the audit unit is the session, not
// the syscall. The first write through this handle begins a mutation
// (refused EIO if it cannot be recorded — the bytes must not land); later
// writes through the same handle are charged only. The completion row is
// Close's job.
func (f *file) WriteAt(p []byte, offset int64) (int, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return 0, toErrno(err)
	}
	if !f.r.policy.Write {
		// Defense in depth: Open already refused a write-mode open on a
		// read-only mount, so this branch should never be reached.
		return 0, linux.EROFS
	}
	if f.open == nil {
		return 0, linux.EINVAL
	}
	if !f.writeSessionOpen {
		end, err := f.r.hooks.BeginMutation("write", map[string]any{"path": f.rel})
		if err != nil {
			return 0, toErrno(err)
		}
		f.writeSessionOpen = true
		f.endWriteSession = end
	}
	n, err := f.open.WriteAt(p, offset)
	if n > 0 {
		if cerr := f.r.hooks.ChargeWrite(n); cerr != nil {
			// The bytes already landed on disk: WriteAt's own count is
			// not known until it returns, so the write-side charge runs
			// after, and the refusal is reported as EDQUOT regardless.
			return n, linux.EDQUOT
		}
	}
	if err != nil {
		return n, toErrno(err)
	}
	return n, nil
}

// Close releases the open handle — whenever one was opened, write session
// or not — and, if a write session was started through this handle, ends
// it (its completion row reflects whether the close itself succeeded).
// Closing always proceeds — no AdmitOp — so a fid cannot leak when a
// budget is spent. Closing a file that was never Opened is a no-op, per
// the interface.
func (f *file) Close() error {
	var cerr error
	if f.open != nil {
		cerr = f.open.Close()
	}
	if f.writeSessionOpen {
		f.writeSessionOpen = false
		end := f.endWriteSession
		f.endWriteSession = nil
		end(cerr)
	}
	if cerr != nil {
		return toErrno(cerr)
	}
	return nil
}

// refuseReadOnly is the read-only half of every mutating method: any
// mutation on a read mount is refused EROFS, enforced here server-side and
// never relying on the client's own -o ro. It fires before BeginMutation —
// there is nothing to record an intent for.
func (f *file) refuseReadOnly(op string, args map[string]any) bool {
	if f.r.policy.Write {
		return false
	}
	f.r.hooks.Refused(op, args, "read-only-mount")
	return true
}

// createFile creates and opens a new regular file under this directory.
// It is the shared body of Create and Mknod(S_IFREG), where "S_IFREG via
// mknod is treated as create". The mutation (and its own intent/
// completion pair) has already been begun by the caller.
func (f *file) createFile(name string, flags p9.OpenFlags, permissions p9.FileMode) (*file, p9.QID, error) {
	mode := maskCreateMode(permissions)
	osFile, err := f.r.root.OpenFile(
		rootPath(path.Join(f.rel, name)),
		flags.OSFlags()|os.O_CREATE|os.O_EXCL,
		mode.OSMode(),
	)
	if err != nil {
		return nil, p9.QID{}, toErrno(err)
	}
	info, statErr := osFile.Stat()
	if statErr != nil {
		if cerr := osFile.Close(); cerr != nil {
			// The Stat failure is the answer; a failed close on top of it
			// is joined rather than silently dropped.
			statErr = errors.Join(statErr, cerr)
		}
		return nil, p9.QID{}, toErrno(statErr)
	}
	return &file{r: f.r, rel: path.Join(f.rel, name), open: osFile}, qidFromInfo(info), nil
}

// Create creates a new regular file and opens it according to the flags.
// The created file is already Open.
func (f *file) Create(name string, flags p9.OpenFlags, permissions p9.FileMode, uid p9.UID, gid p9.GID) (p9.File, p9.QID, uint32, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return nil, p9.QID{}, 0, toErrno(err)
	}
	if f.refuseReadOnly("create", map[string]any{"path": path.Join(f.rel, name)}) {
		return nil, p9.QID{}, 0, linux.EROFS
	}
	end, err := f.r.hooks.BeginMutation("create", map[string]any{"path": path.Join(f.rel, name)})
	if err != nil {
		return nil, p9.QID{}, 0, toErrno(err)
	}
	created, qid, cerr := f.createFile(name, flags, permissions)
	end(cerr)
	if cerr != nil {
		return nil, p9.QID{}, 0, cerr
	}
	// The create intent's completion is separate from any write-session
	// completion the returned file's own WriteAt will later track.
	return created, qid, 0, nil
}

// Mkdir creates a subdirectory.
func (f *file) Mkdir(name string, permissions p9.FileMode, uid p9.UID, gid p9.GID) (p9.QID, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return p9.QID{}, toErrno(err)
	}
	if f.refuseReadOnly("mkdir", map[string]any{"path": path.Join(f.rel, name)}) {
		return p9.QID{}, linux.EROFS
	}
	end, err := f.r.hooks.BeginMutation("mkdir", map[string]any{"path": path.Join(f.rel, name)})
	if err != nil {
		return p9.QID{}, toErrno(err)
	}
	mode := maskCreateMode(permissions)
	cerr := f.r.root.Mkdir(rootPath(path.Join(f.rel, name)), mode.OSMode())
	end(cerr)
	if cerr != nil {
		return p9.QID{}, toErrno(cerr)
	}
	return f.qidOf(path.Join(f.rel, name))
}

// Symlink makes a new symbolic link. The target (oldName) is a string the
// client's kernel will resolve later, against the client's own filesystem;
// this server never interprets or follows it, so it passes through
// unvalidated and unmodified — the creation act itself is what gets
// audited, with the target.
func (f *file) Symlink(oldName string, newName string, uid p9.UID, gid p9.GID) (p9.QID, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return p9.QID{}, toErrno(err)
	}
	args := map[string]any{"path": path.Join(f.rel, newName), "target": oldName}
	if f.refuseReadOnly("symlink", args) {
		return p9.QID{}, linux.EROFS
	}
	end, err := f.r.hooks.BeginMutation("symlink", args)
	if err != nil {
		return p9.QID{}, toErrno(err)
	}
	cerr := f.r.root.Symlink(oldName, rootPath(path.Join(f.rel, newName)))
	end(cerr)
	if cerr != nil {
		return p9.QID{}, toErrno(cerr)
	}
	return f.qidOf(path.Join(f.rel, newName))
}

// Link refuses hardlinks unconditionally — on a read-only mount too, since
// it is refused even on a write one. An alias breaks the audit's path
// model and can reach an existing inode whose other name lies outside the
// root through a name inside it.
func (f *file) Link(target p9.File, newName string) error {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return toErrno(err)
	}
	f.r.hooks.Refused("link", map[string]any{"path": path.Join(f.rel, newName)}, "hardlink-refused")
	return linux.EPERM
}

// Mknod creates a device node. Only S_IFREG is honoured, treated exactly as
// create; char, block, fifo and socket are refused — a device node, a fifo
// or a Unix socket created here is a host object.
func (f *file) Mknod(name string, mode p9.FileMode, major, minor uint32, uid p9.UID, gid p9.GID) (p9.QID, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return p9.QID{}, toErrno(err)
	}
	args := map[string]any{"path": path.Join(f.rel, name), "mode": mode}
	if !mode.IsRegular() {
		f.r.hooks.Refused("mknod", args, "device-node-refused")
		return p9.QID{}, linux.EPERM
	}
	if f.refuseReadOnly("create", map[string]any{"path": path.Join(f.rel, name)}) {
		return p9.QID{}, linux.EROFS
	}
	end, err := f.r.hooks.BeginMutation("create", map[string]any{"path": path.Join(f.rel, name)})
	if err != nil {
		return p9.QID{}, toErrno(err)
	}
	created, qid, cerr := f.createFile(name, p9.WriteOnly, mode)
	end(cerr)
	if cerr != nil {
		return p9.QID{}, cerr
	}
	if created.open != nil {
		// The node already exists by this point, so a failed close must
		// not read as a failed mknod — the client would retry into
		// EEXIST. Checked, deliberately not reported.
		_ = created.open.Close()
	}
	return qid, nil
}

// RenameAt renames a child of this directory into newDir. Both ends are
// walks under this one attach, so neither can name anything outside the
// root by construction; a different *Root (a second mount of the same
// project) is EXDEV. There is no (dev,ino) self-move guard: rename(2) is the
// primitive, and a rename onto itself on a case-insensitive filesystem is a
// real rename, not a self-move.
func (f *file) RenameAt(oldName string, newDir p9.File, newName string) error {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return toErrno(err)
	}
	from := path.Join(f.rel, oldName)
	to := from
	if nd, ok := newDir.(*file); ok {
		to = path.Join(nd.rel, newName)
	}
	if f.refuseReadOnly("rename", map[string]any{"from": from, "to": to}) {
		return linux.EROFS
	}
	nd, ok := newDir.(*file)
	if !ok || nd.r != f.r {
		return linux.EXDEV
	}
	args := map[string]any{"from": from, "to": path.Join(nd.rel, newName)}
	end, err := f.r.hooks.BeginMutation("rename", args)
	if err != nil {
		return toErrno(err)
	}
	cerr := f.r.root.Rename(rootPath(from), rootPath(path.Join(nd.rel, newName)))
	end(cerr)
	if cerr != nil {
		return toErrno(cerr)
	}
	return nil
}

// Rename is never called on the server (the interface's own doc:
// "RenameAt will always be used instead"); it exists only because the
// interface requires the method.
func (f *file) Rename(newDir p9.File, newName string) error {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return toErrno(err)
	}
	return linux.ENOSYS
}

// UnlinkAt removes a child of this directory. The op name is "unlink" or
// "rmdir" depending on the target's own type (a read, checked before the
// mutation begins so the audit row says what actually happened); a
// non-empty directory fails naturally with the kernel's own rmdir
// semantics.
func (f *file) UnlinkAt(name string, flags uint32) error {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return toErrno(err)
	}
	rel := path.Join(f.rel, name)
	args := map[string]any{"path": rel}
	if f.refuseReadOnly("unlink", args) {
		return linux.EROFS
	}
	info, err := f.r.root.Lstat(rootPath(rel))
	if err != nil {
		return toErrno(err)
	}
	op := "unlink"
	if info.IsDir() {
		op = "rmdir"
	}
	end, err := f.r.hooks.BeginMutation(op, args)
	if err != nil {
		return toErrno(err)
	}
	cerr := f.r.root.Remove(rootPath(rel))
	end(cerr)
	if cerr != nil {
		return toErrno(cerr)
	}
	return nil
}

// SetAttr applies requested attribute changes, all-or-nothing. Validation
// of every requested sub-field completes before any of them is applied: a
// chown that is not a no-op refuses the whole call, whatever else it asked
// for. The audit op is the most specific thing actually changing —
// truncate beats chmod, which beats a bare setattr; a call requesting both
// size and mode audits as truncate, the more significant of the two.
func (f *file) SetAttr(valid p9.SetAttrMask, attr p9.SetAttr) error {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return toErrno(err)
	}
	if valid.Empty() {
		return nil
	}
	if f.refuseReadOnly("setattr", map[string]any{"path": f.rel}) {
		return linux.EROFS
	}
	info, err := f.lstatInfo()
	if err != nil {
		return toErrno(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	if valid.UID && p9.UID(st.Uid) != attr.UID {
		f.r.hooks.Refused("chown", map[string]any{"path": f.rel}, "chown-refused")
		return linux.EPERM
	}
	if valid.GID && p9.GID(st.Gid) != attr.GID {
		f.r.hooks.Refused("chown", map[string]any{"path": f.rel}, "chown-refused")
		return linux.EPERM
	}

	var op string
	args := map[string]any{"path": f.rel}
	switch {
	case valid.Size:
		op = "truncate"
		args["size"] = attr.Size
	case valid.Permissions:
		op = "chmod"
		args["mode"] = maskCreateMode(attr.Permissions)
	default:
		op = "setattr"
	}
	end, err := f.r.hooks.BeginMutation(op, args)
	if err != nil {
		return toErrno(err)
	}
	// Each requested field is applied in turn; the first failure stops the
	// rest and is what the completion row and the return value carry.
	var cerr error
	if valid.Size {
		h := f.open
		opened := false
		if h == nil {
			h, cerr = f.r.root.OpenFile(rootPath(f.rel), os.O_WRONLY, 0)
			opened = true
		}
		if cerr == nil {
			cerr = h.Truncate(int64(attr.Size))
		}
		if opened {
			if cerrClose := h.Close(); cerrClose != nil && cerr == nil {
				// The open is short-lived: a failed close is the result
				// only when nothing else has already failed.
				cerr = cerrClose
			}
		}
	}
	if cerr == nil && valid.Permissions {
		cerr = f.r.root.Chmod(rootPath(f.rel), maskCreateMode(attr.Permissions).OSMode())
	}
	if cerr == nil && (valid.ATime || valid.MTime) {
		atime := time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec)
		mtime := time.Unix(st.Mtimespec.Sec, st.Mtimespec.Nsec)
		if valid.ATime {
			atime = time.Unix(int64(attr.ATimeSeconds), int64(attr.ATimeNanoSeconds))
		}
		if valid.MTime {
			mtime = time.Unix(int64(attr.MTimeSeconds), int64(attr.MTimeNanoSeconds))
		}
		cerr = f.r.root.Chtimes(rootPath(f.rel), atime, mtime)
	}
	// CTime is not directly settable; it rides alongside the other
	// changes and is never a reason to refuse the call.
	// No-op UID/GID: already validated as equal to the current owner,
	// so there is nothing to Chown.
	end(cerr)
	if cerr != nil {
		return toErrno(cerr)
	}
	return nil
}

// SetXattr sets an extended attribute, fd-based: the descriptor resolves
// through the Root first, the syscall adds no path resolution of its own.
// com.apple.* names are host-system semantics and refused in both
// directions.
func (f *file) SetXattr(attrName string, data []byte, flags p9.XattrFlags) error {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return toErrno(err)
	}
	args := map[string]any{"path": f.rel, "xattr": attrName}
	if isAppleXattr(attrName) {
		f.r.hooks.Refused("setxattr", args, "apple-xattr")
		return linux.EPERM
	}
	if f.refuseReadOnly("setxattr", args) {
		return linux.EROFS
	}
	end, err := f.r.hooks.BeginMutation("setxattr", args)
	if err != nil {
		return toErrno(err)
	}
	cerr := f.withOpenFD(func(fd int) error {
		return unix.Fsetxattr(fd, attrName, data, int(flags))
	})
	end(cerr)
	if cerr != nil {
		return toErrno(cerr)
	}
	return nil
}

// GetXattr reads one extended attribute, fd-based, probing its size first.
// com.apple.* names are refused even to read.
func (f *file) GetXattr(attrName string) ([]byte, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return nil, toErrno(err)
	}
	if isAppleXattr(attrName) {
		f.r.hooks.Refused("getxattr", map[string]any{"path": f.rel, "xattr": attrName}, "apple-xattr")
		return nil, linux.EPERM
	}
	var data []byte
	err := f.withOpenFD(func(fd int) error {
		size, perr := unix.Fgetxattr(fd, attrName, nil)
		if perr != nil {
			if perr == unix.ENOATTR {
				// No such attribute, in the errno the wire wants.
				return linux.ENODATA
			}
			return perr
		}
		if size == 0 {
			// A zero-length value is a legal xattr; it exists and is
			// simply empty, which is not ENODATA.
			data = []byte{}
			return nil
		}
		buf := make([]byte, size)
		n, rerr := unix.Fgetxattr(fd, attrName, buf)
		if rerr != nil {
			return rerr
		}
		data = buf[:n]
		return nil
	})
	if err != nil {
		return nil, toErrno(err)
	}
	return data, nil
}

// ListXattrs lists the attribute names, fd-based. Apple-prefixed names are
// left in the list on purpose: the spec leaves this to judgment, and
// filtering the list is the only way to make "must not read" consistent with
// "must not be known to exist" — not filtering only leaks that they are
// there, which is less than a read, but the server's own stance is that
// com.apple.* does not exist for this client at all.
func (f *file) ListXattrs() ([]string, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return nil, toErrno(err)
	}
	var names []string
	err := f.withOpenFD(func(fd int) error {
		buf := make([]byte, 64<<10)
		n, rerr := unix.Flistxattr(fd, buf)
		if rerr != nil {
			return rerr
		}
		for _, s := range splitNUL(buf[:n]) {
			if s == "" {
				continue
			}
			if isAppleXattr(s) {
				continue
			}
			names = append(names, s)
		}
		return nil
	})
	if err != nil {
		return nil, toErrno(err)
	}
	return names, nil
}

// RemoveXattr removes an extended attribute, fd-based. com.apple.* names
// are refused, like everywhere else.
func (f *file) RemoveXattr(attrName string) error {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return toErrno(err)
	}
	args := map[string]any{"path": f.rel, "xattr": attrName}
	if isAppleXattr(attrName) {
		f.r.hooks.Refused("removexattr", args, "apple-xattr")
		return linux.EPERM
	}
	if f.refuseReadOnly("removexattr", args) {
		return linux.EROFS
	}
	end, err := f.r.hooks.BeginMutation("removexattr", args)
	if err != nil {
		return toErrno(err)
	}
	cerr := f.withOpenFD(func(fd int) error {
		return unix.Fremovexattr(fd, attrName)
	})
	end(cerr)
	if cerr != nil {
		return toErrno(cerr)
	}
	return nil
}

// Lock refuses: 9P locks are never sent by the intended client, and the
// refusal is the server's so a foreign client cannot reach them either.
func (f *file) Lock(pid int, locktype p9.LockType, flags p9.LockFlags, start, length uint64, client string) (p9.LockStatus, error) {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return 0, toErrno(err)
	}
	return 0, linux.ENOTSUP
}

// FSync flushes the open handle's data. No mutation of its own: the write
// session it belongs to already owns the audit rows.
func (f *file) FSync() error {
	if err := f.r.hooks.AdmitOp(); err != nil {
		return toErrno(err)
	}
	if f.open == nil {
		return linux.EINVAL
	}
	return toErrno(f.open.Sync())
}

// Renamed updates this file's own path after the framework committed a
// rename on another file value. It may not fail; a newDir that is not one of
// ours leaves the path unchanged rather than panicking.
func (f *file) Renamed(newDir p9.File, newName string) {
	if nd, ok := newDir.(*file); ok {
		f.rel = path.Join(nd.rel, newName)
	}
}

// splitNUL cuts a Flistxattr reply (NUL-separated names) into its names.
func splitNUL(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			out = append(out, string(b[start:i]))
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}
