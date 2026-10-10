package files

import (
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
)

// errNotDir is what list and search answer for a path through a symlink: they
// never follow one, and they do not report it as SYMLINK.
var errNotDir = ferr(400, "ENOTDIR", "not a directory")

// resolve walks rel below root one segment at a time and answers symErr at
// the first symlink. A segment that does not exist ends the walk: the op
// itself reports the missing path.
func resolve(root, rel string, symErr *fileErr) (string, *fileErr) {
	cur := root
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" {
			continue
		}
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if err != nil {
			return filepath.Join(root, filepath.FromSlash(rel)), nil
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", symErr
		}
	}
	return cur, nil
}

type entryInfo struct {
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	MtimeMS int64  `json:"mtime_ms"`
}

func infoOf(fi fs.FileInfo) entryInfo {
	t := "file"
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		t = "symlink"
	case fi.IsDir():
		t = "directory"
	}
	return entryInfo{Type: t, Size: fi.Size(), MtimeMS: fi.ModTime().UnixMilli()}
}

func (s *service) list(c *call) (any, *fileErr) {
	var req struct {
		Path       string `json:"path"`
		ShowHidden bool   `json:"show_hidden"`
	}
	if e := c.decode(&req); e != nil {
		return nil, e
	}
	rel, e := c.path(req.Path)
	if e != nil {
		return nil, e
	}
	dir, e := c.abs(rel, errNotDir)
	if e != nil {
		return nil, e
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, c.osErr(err)
	}
	type entry struct {
		Name string `json:"name"`
		entryInfo
	}
	entries := []entry{}
	for _, de := range des {
		if !req.ShowHidden && strings.HasPrefix(de.Name(), ".") {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		entries = append(entries, entry{de.Name(), infoOf(fi)})
	}
	return map[string]any{"entries": entries}, nil
}

func (s *service) stat(c *call) (any, *fileErr) {
	var req struct {
		Path string `json:"path"`
	}
	if e := c.decode(&req); e != nil {
		return nil, e
	}
	rel, e := c.path(req.Path)
	if e != nil {
		return nil, e
	}
	p, e := c.abs(rel, errSymlink)
	if e != nil {
		return nil, e
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, c.osErr(err)
	}
	return infoOf(fi), nil
}

func (s *service) read(c *call) (any, *fileErr) {
	var req struct {
		Path     string `json:"path"`
		MaxBytes int64  `json:"max_bytes"`
	}
	if e := c.decode(&req); e != nil {
		return nil, e
	}
	rel, e := c.path(req.Path)
	if e != nil {
		return nil, e
	}
	p, e := c.abs(rel, errSymlink)
	if e != nil {
		return nil, e
	}
	limit := req.MaxBytes
	if limit <= 0 || limit > maxFileBytes {
		limit = maxFileBytes
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, c.osErr(err)
	}
	if fi.IsDir() {
		return nil, ferr(400, "EISDIR", "Path is a directory")
	}
	if fi.Size() > limit {
		return nil, errTooLarge("File too large", fi.Size())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, c.osErr(err)
	}
	return map[string]any{"content": string(b), "size": len(b)}, nil
}

func (s *service) write(c *call) (any, *fileErr) {
	var req struct {
		Path       string `json:"path"`
		Content    string `json:"content"`
		Encoding   string `json:"encoding"`
		CreateOnly bool   `json:"create_only"`
	}
	if e := c.decode(&req); e != nil {
		return nil, e
	}
	rel, e := c.path(req.Path)
	if e != nil {
		return nil, e
	}
	if rel == "" {
		return nil, ferr(400, "EISDIR", "Path is a directory")
	}
	data := []byte(req.Content)
	switch req.Encoding {
	case "", "utf8":
		req.Encoding = "utf8"
	case "base64":
		var err error
		if data, err = base64.StdEncoding.DecodeString(req.Content); err != nil {
			return nil, errInvalid("content is not base64")
		}
	default:
		return nil, errInvalid("encoding must be utf8 or base64")
	}
	if len(data) > maxFileBytes {
		return nil, errTooLarge("File too large", int64(len(data)))
	}
	args := map[string]any{"path": rel, "host_id": c.t.hostID, "bytes": len(data), "encoding": req.Encoding, "create_only": req.CreateOnly}
	var created bool
	resp, e := c.mutate(args, []string{rel}, func() (any, *fileErr) {
		p, e := c.abs(rel, errSymlink)
		if e != nil {
			return nil, e
		}
		flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if req.CreateOnly {
			flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
		}
		_, statErr := os.Lstat(p)
		created = statErr != nil
		f, err := os.OpenFile(p, flags, 0o644)
		if err != nil {
			return nil, c.osErr(err)
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, c.osErr(err)
		}
		return map[string]string{"path": rel}, nil
	})
	if e == nil {
		kind := "change"
		if created {
			kind = "rename"
		}
		c.effects = append(c.effects, fsEvent{rel, kind})
	}
	return resp, e
}

func (s *service) mkdir(c *call) (any, *fileErr) {
	var req struct {
		Parent string `json:"parent"`
		Name   string `json:"name"`
	}
	if e := c.decode(&req); e != nil {
		return nil, e
	}
	parent, e := c.path(req.Parent)
	if e != nil {
		return nil, e
	}
	if e := validName(req.Name); e != nil {
		return nil, e
	}
	rel := join(parent, req.Name)
	args := map[string]any{"path": rel, "host_id": c.t.hostID}
	resp, e := c.mutate(args, []string{parent}, func() (any, *fileErr) {
		p, e := c.abs(rel, errSymlink)
		if e != nil {
			return nil, e
		}
		if err := os.Mkdir(p, 0o755); err != nil {
			return nil, c.osErr(err)
		}
		return map[string]string{"path": rel}, nil
	})
	if e == nil {
		c.effects = append(c.effects, fsEvent{rel, "rename"})
	}
	return resp, e
}

func (s *service) rename(c *call) (any, *fileErr) {
	var req struct {
		Path    string `json:"path"`
		NewName string `json:"new_name"`
	}
	if e := c.decode(&req); e != nil {
		return nil, e
	}
	src, e := c.path(req.Path)
	if e != nil {
		return nil, e
	}
	if e := validName(req.NewName); e != nil {
		return nil, e
	}
	if src == "" {
		return nil, errInvalid("cannot rename the project root")
	}
	dir := path.Dir(src)
	if dir == "." {
		dir = ""
	}
	return s.relocate(c, "rename", src, join(dir, req.NewName))
}

func (s *service) move(c *call) (any, *fileErr) {
	var req struct {
		Path    string `json:"path"`
		DestDir string `json:"dest_dir"`
	}
	if e := c.decode(&req); e != nil {
		return nil, e
	}
	src, e := c.path(req.Path)
	if e != nil {
		return nil, e
	}
	dir, e := c.path(req.DestDir)
	if e != nil {
		return nil, e
	}
	if src == "" {
		return nil, errInvalid("cannot move the project root")
	}
	if dir == src || strings.HasPrefix(dir, src+"/") {
		return nil, errInvalid("cannot move a folder into itself")
	}
	return s.relocate(c, "move", src, join(dir, path.Base(src)))
}

// relocate renames src to dst. An existing destination refuses with EEXIST,
// except the same entry under a new case.
func (s *service) relocate(c *call, tool, src, dst string) (any, *fileErr) {
	args := map[string]any{"path": src, "new_path": dst, "host_id": c.t.hostID}
	parent := path.Dir(dst)
	if parent == "." {
		parent = ""
	}
	resp, e := c.mutate(args, []string{src, parent}, func() (any, *fileErr) {
		from, e := c.abs(src, errSymlink)
		if e != nil {
			return nil, e
		}
		to, e := c.abs(dst, errSymlink)
		if e != nil {
			return nil, e
		}
		fi, err := os.Lstat(from)
		if err != nil {
			return nil, c.osErr(err)
		}
		if tf, err := os.Lstat(to); err == nil && !os.SameFile(fi, tf) {
			return nil, ferr(409, "EEXIST", "Already exists")
		}
		if tool == "move" {
			di, err := os.Stat(filepath.Dir(to))
			if err != nil {
				return nil, c.osErr(err)
			}
			if !di.IsDir() {
				return nil, errNotDir
			}
		}
		if err := os.Rename(from, to); err != nil {
			return nil, c.osErr(err)
		}
		return map[string]string{"path": dst}, nil
	})
	if e == nil {
		c.effects = append(c.effects, fsEvent{src, "rename"}, fsEvent{dst, "rename"})
	}
	return resp, e
}

func (s *service) remove(c *call) (any, *fileErr) {
	var req struct {
		Path string `json:"path"`
	}
	if e := c.decode(&req); e != nil {
		return nil, e
	}
	rel, e := c.path(req.Path)
	if e != nil {
		return nil, e
	}
	if rel == "" {
		return nil, errInvalid("cannot delete the project root")
	}
	trashed := c.t.hostID == ""
	args := map[string]any{"path": rel, "host_id": c.t.hostID, "trashed": trashed}
	resp, e := c.mutate(args, []string{rel}, func() (any, *fileErr) {
		p, e := c.abs(rel, errSymlink)
		if e != nil {
			return nil, e
		}
		if _, err := os.Lstat(p); err != nil {
			return nil, c.osErr(err)
		}
		var err error
		if trashed {
			err = s.trash(p)
		} else {
			err = os.RemoveAll(p)
		}
		if err != nil {
			return nil, c.osErr(err)
		}
		return map[string]bool{"trashed": trashed}, nil
	})
	if e == nil {
		c.effects = append(c.effects, fsEvent{rel, "rename"})
	}
	return resp, e
}

// trash moves p under DIR/trash. Across filesystems it copies, then removes.
func (s *service) trash(p string) error {
	dst := filepath.Join(s.d.Dir, "trash", events.NewID()[:8]+"-"+filepath.Base(p))
	err := os.Rename(p, dst)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyTree(p, dst); err != nil {
		return err
	}
	return os.RemoveAll(p)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(to, fi.Mode().Perm())
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(target, to)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	})
}
