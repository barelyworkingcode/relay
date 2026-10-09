package projectfs

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// local is the console backend: a project root on this machine's disk.
//
// This is deliberate: every path is opened with openat from a directory
// descriptor and O_NOFOLLOW_ANY, which makes the kernel refuse a symbolic
// link in any component of the relative path. There is no check-then-use
// window for a session that plants links inside its own project folder. The
// root path itself may contain links (a project under /tmp); only what lies
// below it is held to the rule.
type local struct {
	root string
}

// NewLocal returns the console backend for root, which must be an existing
// directory.
func NewLocal(root string) (Backend, error) {
	if root == "" {
		return nil, Errf(CodeNotAvailable, "project has no path")
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, mapErr(err)
	}
	_ = unix.Close(fd)
	return &local{root: root}, nil
}

// mapErr turns an OS error into the wire error. ELOOP is the kernel's answer
// to O_NOFOLLOW_ANY meeting a symbolic link.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var fe *Error
	if errors.As(err, &fe) {
		return err
	}
	var errno unix.Errno
	if !errors.As(err, &errno) {
		return Errf(CodeError, err.Error())
	}
	switch errno {
	case unix.ELOOP:
		return Errf(CodeSymlink, "Symbolic links are not opened")
	case unix.EACCES, unix.EPERM, unix.EROFS:
		return Errf(CodeEACCES, "Permission denied")
	case unix.ENOENT:
		return Errf(CodeENOENT, "Not found")
	case unix.EISDIR:
		return Errf(CodeEISDIR, "Path is a directory")
	case unix.ENOTDIR:
		return Errf(CodeENOTDIR, "Not a directory")
	case unix.EEXIST, unix.ENOTEMPTY:
		return Errf(CodeEEXIST, "Already exists")
	case unix.EINVAL:
		return Errf(CodeInvalid, "Invalid operation")
	case unix.ENAMETOOLONG:
		return Errf(CodeInvalid, "Name too long")
	}
	return Errf(CodeError, errno.Error())
}

func (l *local) openRoot() (int, error) {
	fd, err := unix.Open(l.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	return fd, mapErr(err)
}

// openDir opens the directory at rel (the root for "").
func (l *local) openDir(rel string) (int, error) {
	rootFd, err := l.openRoot()
	if err != nil {
		return -1, err
	}
	if rel == "" {
		return rootFd, nil
	}
	defer unix.Close(rootFd)
	fd, err := unix.Openat(rootFd, rel, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW_ANY|unix.O_CLOEXEC, 0)
	return fd, mapErr(err)
}

// openParent opens the directory holding rel and returns it with rel's last
// segment. rel must not be the root.
func (l *local) openParent(rel string) (dirFd int, name string, err error) {
	if rel == "" {
		return -1, "", Errf(CodeInvalid, "operation needs a path below the project root")
	}
	dir, name := splitRel(rel)
	dirFd, err = l.openDir(dir)
	return dirFd, name, err
}

func splitRel(rel string) (dir, name string) {
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return "", rel
	}
	return rel[:i], rel[i+1:]
}

func lstatAt(dirFd int, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	return st, mapErr(err)
}

func typeOf(mode uint16) string {
	switch uint32(mode) & unix.S_IFMT {
	case unix.S_IFDIR:
		return TypeDirectory
	case unix.S_IFLNK:
		return TypeSymlink
	}
	return TypeFile
}

func infoOf(st *unix.Stat_t) Info {
	return Info{Type: typeOf(st.Mode), Size: st.Size, MtimeMs: st.Mtim.Sec*1000 + st.Mtim.Nsec/1e6}
}

// lstatPath is lstat of rel with no link allowed in any component, the final
// one included.
func (l *local) lstatPath(rel string) (unix.Stat_t, error) {
	if rel == "" {
		fd, err := l.openRoot()
		if err != nil {
			return unix.Stat_t{}, err
		}
		defer unix.Close(fd)
		var st unix.Stat_t
		return st, mapErr(unix.Fstat(fd, &st))
	}
	dirFd, name, err := l.openParent(rel)
	if err != nil {
		return unix.Stat_t{}, err
	}
	defer unix.Close(dirFd)
	return lstatAt(dirFd, name)
}

func (l *local) List(ctx context.Context, rel string, showHidden bool) ([]Entry, error) {
	fd, err := l.openDir(rel)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "dir")
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]Entry, 0, len(names))
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !showHidden && strings.HasPrefix(n, ".") {
			continue
		}
		st, err := lstatAt(fd, n)
		if err != nil {
			continue // removed while listing
		}
		in := infoOf(&st)
		out = append(out, Entry{Name: n, Type: in.Type, Size: in.Size, MtimeMs: in.MtimeMs})
	}
	return out, nil
}

func (l *local) Stat(_ context.Context, rel string) (Info, error) {
	st, err := l.lstatPath(rel)
	if err != nil {
		return Info{}, err
	}
	if uint32(st.Mode)&unix.S_IFMT == unix.S_IFLNK {
		return Info{}, Errf(CodeSymlink, "Symbolic links are not opened")
	}
	return infoOf(&st), nil
}

// openFile opens a regular file read-only. O_NONBLOCK keeps a FIFO from
// blocking the open; it is refused as not a regular file just after.
func (l *local) openFile(rel string) (*os.File, Info, error) {
	dirFd, name, err := l.openParent(rel)
	if err != nil {
		if rel == "" {
			return nil, Info{}, Errf(CodeEISDIR, "Path is a directory")
		}
		return nil, Info{}, err
	}
	defer unix.Close(dirFd)
	fd, err := unix.Openat(dirFd, name, unix.O_RDONLY|unix.O_NOFOLLOW_ANY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, Info{}, mapErr(err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, Info{}, mapErr(err)
	}
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFREG:
	case unix.S_IFDIR:
		_ = unix.Close(fd)
		return nil, Info{}, Errf(CodeEISDIR, "Path is a directory")
	default:
		_ = unix.Close(fd)
		return nil, Info{}, Errf(CodeInvalid, "Not a regular file")
	}
	return os.NewFile(uintptr(fd), name), infoOf(&st), nil
}

func (l *local) Read(_ context.Context, rel string, maxBytes int64) (string, int64, error) {
	if maxBytes <= 0 || maxBytes > MaxReadBytes {
		maxBytes = MaxReadBytes
	}
	f, info, err := l.openFile(rel)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	if info.Size > maxBytes {
		return "", 0, &Error{Code: CodeTooLarge, Msg: "File too large", Size: info.Size}
	}
	// The file may grow after fstat; the limit reader bounds the copy.
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return "", 0, mapErr(err)
	}
	if int64(len(b)) > maxBytes {
		return "", 0, &Error{Code: CodeTooLarge, Msg: "File too large", Size: int64(len(b))}
	}
	return string(b), int64(len(b)), nil
}

func (l *local) Open(_ context.Context, rel string) (io.ReadCloser, Info, error) {
	f, info, err := l.openFile(rel)
	if err != nil {
		return nil, Info{}, err
	}
	return f, info, nil
}

func (l *local) Write(_ context.Context, rel string, data []byte, o WriteOpts) error {
	if int64(len(data)) > MaxWriteBytes {
		return &Error{Code: CodeTooLarge, Msg: "File too large", Size: int64(len(data))}
	}
	dirFd, name, err := l.openParent(rel)
	if err != nil {
		if rel == "" {
			return Errf(CodeEISDIR, "Path is a directory")
		}
		return err
	}
	defer unix.Close(dirFd)
	flags := unix.O_WRONLY | unix.O_CREAT | unix.O_NOFOLLOW_ANY | unix.O_NONBLOCK | unix.O_CLOEXEC
	if o.CreateOnly {
		flags |= unix.O_EXCL
	}
	fd, err := unix.Openat(dirFd, name, flags, 0o644)
	if err != nil {
		return mapErr(err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return mapErr(err)
	}
	if uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG {
		return Errf(CodeInvalid, "Not a regular file")
	}
	// Truncate after the type check so a FIFO is never truncated and a
	// failed open leaves the old content alone.
	if err := f.Truncate(0); err != nil {
		return mapErr(err)
	}
	if _, err := f.Write(data); err != nil {
		return mapErr(err)
	}
	return mapErr(f.Close())
}

func (l *local) Mkdir(_ context.Context, parentRel, name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	dirFd, err := l.openDir(parentRel)
	if err != nil {
		return "", err
	}
	defer unix.Close(dirFd)
	if err := unix.Mkdirat(dirFd, name, 0o755); err != nil {
		return "", mapErr(err)
	}
	return JoinRel(parentRel, name), nil
}

func (l *local) Rename(_ context.Context, rel, newName string) (string, error) {
	if err := ValidateName(newName); err != nil {
		return "", err
	}
	dirFd, name, err := l.openParent(rel)
	if err != nil {
		return "", err
	}
	defer unix.Close(dirFd)
	if err := moveNoReplace(dirFd, name, dirFd, newName); err != nil {
		return "", err
	}
	dir, _ := splitRel(rel)
	return JoinRel(dir, newName), nil
}

func (l *local) Move(_ context.Context, rel, destDirRel string) (string, error) {
	srcFd, name, err := l.openParent(rel)
	if err != nil {
		return "", err
	}
	defer unix.Close(srcFd)
	dstFd, err := l.openDir(destDirRel)
	if err != nil {
		return "", err
	}
	defer unix.Close(dstFd)
	if err := moveNoReplace(srcFd, name, dstFd, name); err != nil {
		return "", err
	}
	return JoinRel(destDirRel, name), nil
}

// moveNoReplace renames from to to without ever replacing an existing entry,
// and refuses a symbolic link as the source.
//
// This is subtle: RENAME_EXCL makes "destination is free" and "rename" one
// kernel step. The one exception is a case-only rename of the same inode,
// which on a case-insensitive volume looks like a collision with itself and
// is retried as a plain rename.
func moveNoReplace(srcFd int, from string, dstFd int, to string) error {
	src, err := lstatAt(srcFd, from)
	if err != nil {
		return err
	}
	if uint32(src.Mode)&unix.S_IFMT == unix.S_IFLNK {
		return Errf(CodeSymlink, "Symbolic links are not opened")
	}
	err = unix.RenameatxNp(srcFd, from, dstFd, to, unix.RENAME_EXCL)
	if errors.Is(err, unix.EEXIST) {
		if dst, derr := lstatAt(dstFd, to); derr == nil &&
			dst.Dev == src.Dev && dst.Ino == src.Ino && srcFd == dstFd && strings.EqualFold(from, to) && from != to {
			err = unix.Renameat(srcFd, from, dstFd, to)
		}
	}
	return mapErr(err)
}

func (l *local) Delete(_ context.Context, rel string) (bool, error) {
	dirFd, name, err := l.openParent(rel)
	if err != nil {
		return false, err
	}
	defer unix.Close(dirFd)
	st, err := lstatAt(dirFd, name)
	if err != nil {
		return false, err
	}
	if uint32(st.Mode)&unix.S_IFMT == unix.S_IFLNK {
		return false, Errf(CodeSymlink, "Symbolic links are not opened")
	}
	parentPath, err := fdPath(dirFd)
	if err != nil {
		return false, err
	}
	if err := trashPath(parentPath + "/" + name); err != nil {
		return false, err
	}
	return true, nil
}

// fdPath is the canonical path of an open descriptor. Built from a
// descriptor that was opened with O_NOFOLLOW_ANY, it has no link left in it.
func fdPath(fd int) (string, error) {
	buf := make([]byte, unix.PathMax)
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&buf[0])))); err != nil {
		return "", mapErr(err)
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n]), nil
}

func closeFd(fd int) { _ = unix.Close(fd) }
