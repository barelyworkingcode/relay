package main

import (
	"context"
	"encoding/base64"
	"io"
	"path"
	"regexp"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/projectfs"
)

// FileHostStatus is one host agent's connection state, as /ws/files reports
// it.
type FileHostStatus = projectfs.HostStatus

// FileHostPool is what FileOps needs from the host agents; *projectfs.HostPool
// satisfies it.
type FileHostPool interface {
	Backend(h config.Host, root string) projectfs.Backend
	PasteTmp(ctx context.Context, h config.Host, name string, data []byte) (string, error)
	Subscribe(fn func(FileHostStatus)) (cancel func())
	Statuses() []FileHostStatus
}

// FileOps is the one core behind the file routes and /ws/files (docs/
// project-files.md). It resolves the project from fresh settings on every
// call, so a change to files_read_only or a project's path applies to the
// next request.
type FileOps struct {
	Store config.SettingsStore
	Audit *audit.AuditRecorder

	// Hosts serves host projects. Nil answers HOST_UNREACHABLE for them.
	Hosts FileHostPool

	// NewLocal builds the console backend; nil means projectfs.NewLocal.
	NewLocal func(root string) (projectfs.Backend, error)
	// WatchRecheck is how often /ws/files compares watched projects with
	// settings; zero means two seconds.
	WatchRecheck time.Duration

	hubOnce sync.Once
	hub     *watchHub
}

func (o *FileOps) audit() fileAudit { return fileAudit{rec: o.Audit} }

// fileSession is one request's resolved project.
type fileSession struct {
	o     *FileOps
	proj  config.Project
	host  *config.Host
	actor audit.AuditActor
}

// open resolves a project and enforces the first two checks of the order:
// project, then kind.
func (o *FileOps) open(actor audit.AuditActor, projectID string) (*fileSession, error) {
	s := config.FreshSettings(o.Store)
	if s == nil {
		return nil, projectfs.Errf(projectfs.CodeProjectNotFound, "project not found")
	}
	p, _ := config.FindProjectByID(s, projectID)
	if p == nil {
		return nil, projectfs.Errf(projectfs.CodeProjectNotFound, "project not found")
	}
	if p.IsRemote() || p.Path == "" {
		return nil, projectfs.Errf(projectfs.CodeNotAvailable, "files are not available for this project")
	}
	fs := &fileSession{o: o, proj: *p, actor: actor}
	fs.actor.ProjectID = p.ID
	fs.actor.ProjectName = p.Name
	if p.HostID != "" {
		if h, _ := config.FindHostByID(s, p.HostID); h != nil {
			hc := *h
			fs.host = &hc
		}
	}
	return fs, nil
}

// backend returns the project's Backend. A hosted project whose host record
// is gone, or with no host pool wired, is unreachable rather than a console
// path: its Path means nothing on this machine.
func (fs *fileSession) backend() (projectfs.Backend, error) {
	if fs.proj.HostID != "" {
		if fs.host == nil || fs.o.Hosts == nil {
			return nil, projectfs.Errf(projectfs.CodeHostUnreachable, "host is not connected")
		}
		return fs.o.Hosts.Backend(*fs.host, fs.proj.Path), nil
	}
	if fs.o.NewLocal != nil {
		return fs.o.NewLocal(fs.proj.Path)
	}
	return projectfs.NewLocal(fs.proj.Path)
}

func (fs *fileSession) row(tool string, args map[string]any) fileOpRow {
	if args == nil {
		args = map[string]any{}
	}
	args["host_id"] = fs.proj.HostID
	return fileOpRow{actor: fs.actor, root: fs.proj.Path, tool: tool, args: args}
}

// refuse records a denied row for a mutation the boundary refused and
// returns the error unchanged.
func (fs *fileSession) refuse(tool string, args map[string]any, err error) error {
	if code := projectfs.CodeOf(err); deniedCode(code) {
		fs.o.audit().denied(fs.row(tool, args), code)
	}
	return err
}

// gate runs the checks between the lexical path and the backend call: the
// read-only flag, then a symlink probe of every path the mutation touches,
// then the intent row. The probe exists so a refused link yields one denied
// row; the backend still enforces the rule itself at the open.
func (fs *fileSession) gate(ctx context.Context, tool string, args map[string]any, probes ...string) (projectfs.Backend, func(error), error) {
	if fs.proj.FilesReadOnly {
		return nil, nil, fs.refuse(tool, args, projectfs.Errf(projectfs.CodeReadOnly, "This project is read-only"))
	}
	b, err := fs.backend()
	if err != nil {
		return nil, nil, err
	}
	for _, p := range probes {
		if _, perr := b.Stat(ctx, p); projectfs.CodeOf(perr) == projectfs.CodeSymlink {
			return nil, nil, fs.refuse(tool, args, perr)
		}
	}
	end, err := fs.o.audit().begin(fs.row(tool, args))
	if err != nil {
		return nil, nil, err
	}
	return b, end, nil
}

func parentOf(rel string) string {
	d := path.Dir(rel)
	if d == "." {
		return ""
	}
	return d
}

// reader runs a read: no read-only check, no audit.
func (fs *fileSession) reader(rawPath string) (projectfs.Backend, string, error) {
	rel, err := projectfs.CleanRel(rawPath)
	if err != nil {
		return nil, "", err
	}
	b, err := fs.backend()
	return b, rel, err
}

func (fs *fileSession) List(ctx context.Context, p string, showHidden bool) ([]projectfs.Entry, error) {
	b, rel, err := fs.reader(p)
	if err != nil {
		return nil, err
	}
	entries, err := b.List(ctx, rel, showHidden)
	if entries == nil && err == nil {
		entries = []projectfs.Entry{}
	}
	return entries, err
}

func (fs *fileSession) Stat(ctx context.Context, p string) (projectfs.Info, error) {
	b, rel, err := fs.reader(p)
	if err != nil {
		return projectfs.Info{}, err
	}
	return b.Stat(ctx, rel)
}

func (fs *fileSession) Read(ctx context.Context, p string, maxBytes int64) (string, int64, error) {
	b, rel, err := fs.reader(p)
	if err != nil {
		return "", 0, err
	}
	if maxBytes <= 0 || maxBytes > projectfs.MaxReadBytes {
		maxBytes = projectfs.MaxReadBytes
	}
	return b.Read(ctx, rel, maxBytes)
}

func (fs *fileSession) Open(ctx context.Context, p string) (io.ReadCloser, projectfs.Info, error) {
	b, rel, err := fs.reader(p)
	if err != nil {
		return nil, projectfs.Info{}, err
	}
	return b.Open(ctx, rel)
}

func (fs *fileSession) Search(ctx context.Context, o projectfs.SearchOpts) ([]projectfs.Match, bool, error) {
	if _, err := projectfs.NewSearchMatcher(o); err != nil {
		return nil, false, err
	}
	b, err := fs.backend()
	if err != nil {
		return nil, false, err
	}
	m, truncated, err := b.Search(ctx, o)
	if m == nil && err == nil {
		m = []projectfs.Match{}
	}
	return m, truncated, err
}

func (fs *fileSession) Git(ctx context.Context, cwd string, args []string, maxBytes int64) (projectfs.GitResult, error) {
	rel, err := projectfs.CleanRel(cwd)
	if err != nil {
		return projectfs.GitResult{}, err
	}
	if err := projectfs.ValidateGitArgs(args); err != nil {
		return projectfs.GitResult{}, err
	}
	b, err := fs.backend()
	if err != nil {
		return projectfs.GitResult{}, err
	}
	return b.Git(ctx, rel, args, maxBytes)
}

func (fs *fileSession) Write(ctx context.Context, p string, data []byte, encoding string, createOnly bool) (string, error) {
	args := map[string]any{"path": p, "bytes": len(data), "encoding": encoding, "create_only": createOnly}
	rel, err := projectfs.CleanRel(p)
	if err != nil {
		return "", fs.refuse("write", args, err)
	}
	if rel == "" {
		return "", projectfs.Errf(projectfs.CodeEISDIR, "Path is a directory")
	}
	if int64(len(data)) > projectfs.MaxWriteBytes {
		return "", &projectfs.Error{Code: projectfs.CodeTooLarge, Msg: "File too large", Size: int64(len(data))}
	}
	args["path"] = rel
	b, end, err := fs.gate(ctx, "write", args, rel)
	if err != nil {
		return "", err
	}
	err = b.Write(ctx, rel, data, projectfs.WriteOpts{Encoding: encoding, CreateOnly: createOnly})
	end(err)
	return rel, err
}

func (fs *fileSession) Mkdir(ctx context.Context, parent, name string) (string, error) {
	args := map[string]any{"path": projectfs.JoinRel(parent, name)}
	rel, err := projectfs.CleanRel(parent)
	if err != nil {
		return "", fs.refuse("mkdir", args, err)
	}
	if err := projectfs.ValidateName(name); err != nil {
		return "", err
	}
	args["path"] = projectfs.JoinRel(rel, name)
	b, end, err := fs.gate(ctx, "mkdir", args, rel)
	if err != nil {
		return "", err
	}
	out, err := b.Mkdir(ctx, rel, name)
	end(err)
	return out, err
}

func (fs *fileSession) Rename(ctx context.Context, p, newName string) (string, error) {
	args := map[string]any{"path": p}
	rel, err := projectfs.CleanRel(p)
	if err != nil {
		return "", fs.refuse("rename", args, err)
	}
	if err := projectfs.ValidateName(newName); err != nil {
		return "", err
	}
	if rel == "" {
		return "", projectfs.Errf(projectfs.CodeInvalid, "cannot rename the project root")
	}
	args["path"], args["new_path"] = rel, projectfs.JoinRel(parentOf(rel), newName)
	b, end, err := fs.gate(ctx, "rename", args, rel)
	if err != nil {
		return "", err
	}
	out, err := b.Rename(ctx, rel, newName)
	end(err)
	return out, err
}

func (fs *fileSession) Move(ctx context.Context, p, destDir string) (string, error) {
	args := map[string]any{"path": p}
	rel, err := projectfs.CleanRel(p)
	if err != nil {
		return "", fs.refuse("move", args, err)
	}
	dest, err := projectfs.CleanRel(destDir)
	if err != nil {
		return "", fs.refuse("move", args, err)
	}
	if rel == "" {
		return "", projectfs.Errf(projectfs.CodeInvalid, "cannot move the project root")
	}
	args["path"], args["new_path"] = rel, projectfs.JoinRel(dest, path.Base(rel))
	b, end, err := fs.gate(ctx, "move", args, rel, dest)
	if err != nil {
		return "", err
	}
	out, err := b.Move(ctx, rel, dest)
	end(err)
	return out, err
}

func (fs *fileSession) Delete(ctx context.Context, p string) (bool, error) {
	args := map[string]any{"path": p}
	rel, err := projectfs.CleanRel(p)
	if err != nil {
		return false, fs.refuse("delete", args, err)
	}
	if rel == "" {
		return false, projectfs.Errf(projectfs.CodeInvalid, "cannot delete the project root")
	}
	args["path"], args["trashed"] = rel, fs.proj.HostID == ""
	b, end, err := fs.gate(ctx, "delete", args, rel)
	if err != nil {
		return false, err
	}
	trashed, err := b.Delete(ctx, rel)
	end(err)
	return trashed, err
}

var pasteNameRE = regexp.MustCompile(`^eve-paste-[0-9]+-[0-9a-f]+\.(png|jpg|gif|webp)$`)

// PasteTmp writes a pasted image into a host's temp directory. It has no
// project, so it carries no read-only flag and its audit row has none of the
// project fields.
func (o *FileOps) PasteTmp(ctx context.Context, actor audit.AuditActor, hostID, name, dataB64 string) (string, error) {
	s := config.FreshSettings(o.Store)
	var host *config.Host
	if s != nil {
		host, _ = config.FindHostByID(s, hostID)
	}
	if host == nil {
		return "", projectfs.Errf(projectfs.CodeHostNotFound, "host not found")
	}
	if !pasteNameRE.MatchString(name) {
		return "", projectfs.Errf(projectfs.CodeInvalid, "invalid paste file name")
	}
	data, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return "", projectfs.Errf(projectfs.CodeInvalid, "data_b64 is not base64")
	}
	if int64(len(data)) > projectfs.MaxPasteBytes {
		return "", &projectfs.Error{Code: projectfs.CodeTooLarge, Msg: "File too large", Size: int64(len(data))}
	}
	if o.Hosts == nil {
		return "", projectfs.Errf(projectfs.CodeHostUnreachable, "host is not connected")
	}
	end, err := o.audit().begin(fileOpRow{actor: actor, tool: "pastetmp",
		args: map[string]any{"host_id": hostID, "name": name, "bytes": len(data)}})
	if err != nil {
		return "", err
	}
	out, err := o.Hosts.PasteTmp(ctx, *host, name, data)
	end(err)
	return out, err
}
