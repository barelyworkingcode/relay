package main

// plan-broker-and-sessions.md §2 C3 and spec-session-host.md §4.3/§8.3: a
// tokenless caller authenticates by BEING a live session's root process, or
// a kernel-verified descendant of one. Nothing here asserts against a
// caller's own account of itself — the fixture below declares a process tree
// and relay walks it, exactly as it walks the kernel's in production, and
// the darwin file beside this one repeats the load-bearing cases against
// real spawned processes.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
)

// procTable is a fake membership.Source: the process tree a test declares,
// read the same way the darwin Source reads the kernel's.
type procTable struct {
	mu    sync.Mutex
	infos map[int]membership.ProcInfo
	// reads counts Info calls, so a test can tell one ancestry walk from
	// two — the difference between C3's per-connection cache working and a
	// syscall walk on every request.
	reads int
}

func newProcTable() *procTable { return &procTable{infos: map[int]membership.ProcInfo{}} }

func (p *procTable) Info(pid int) (membership.ProcInfo, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	info, ok := p.infos[pid]
	return info, ok
}

func (p *procTable) readCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads
}

func (p *procTable) set(info membership.ProcInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.infos[info.PID] = info
}

func (p *procTable) remove(pid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.infos, pid)
}

// sessionFixture is a router whose membership answers come from a declared
// process tree instead of the kernel, plus the launch table those sessions
// live in.
type sessionFixture struct {
	r     *appRouter
	procs *procTable
	// baseSec anchors every declared start time. Well before acceptedAt, so
	// a process is "older than the connection" unless a test says otherwise.
	baseSec    int64
	acceptedAt time.Time
	hostPID    int
	nextPID    int

	mu     sync.Mutex
	onExit map[int]func()
}

const (
	fixtureHostPID = 900000
	// A pid the fixture never declares, for the "peer relay cannot read"
	// case — an unreadable pid is not a member, it is not an error.
	unknownPID = 999111
)

func newSessionFixture(t *testing.T, s *config.Settings, mgr *mcpbroker.Manager) *sessionFixture {
	t.Helper()
	r := newTestRouter(t, s, mgr)
	r.launches = service.NewLaunches()

	fx := &sessionFixture{
		r:       r,
		procs:   newProcTable(),
		baseSec: time.Now().Add(-time.Hour).Unix(),
		hostPID: fixtureHostPID,
		nextPID: 910000,
		onExit:  map[int]func(){},
	}
	fx.acceptedAt = time.Unix(fx.baseSec+1000, 0)
	r.membership = &membershipAuth{launches: r.launches, src: fx.procs, hostPID: fx.hostPID}

	// The launch table reads the same tree at Bind, so a root's pinned start
	// time and the walk's view of it can never disagree for a reason the
	// fixture invented.
	r.launches.SetRootSourceForTest(fx.procs)
	r.launches.SetRootWatcherForTest(func(pid int, want membership.ProcInfo, onExit func()) (func(), error) {
		got, ok := fx.procs.Info(pid)
		if !ok || got != want {
			return nil, membership.ErrExited
		}
		fx.mu.Lock()
		fx.onExit[pid] = onExit
		fx.mu.Unlock()
		return func() {
			fx.mu.Lock()
			delete(fx.onExit, pid)
			fx.mu.Unlock()
		}, nil
	})
	return fx
}

// spawn declares a process started `age` seconds after baseSec, so a caller
// choosing a larger age for a child than its parent keeps the tree's start
// times in the order a real kernel would report.
func (fx *sessionFixture) spawn(ppid int, age int64) int {
	fx.nextPID++
	pid := fx.nextPID
	fx.procs.set(membership.ProcInfo{PID: pid, PPID: ppid, StartSec: fx.baseSec + age, StartUsec: 0})
	return pid
}

// startSession begins and binds a project_session launch whose root is a
// freshly declared child of relay's own pid — the shape R-S1's Bind produces
// for a real shim's Hello.
func (fx *sessionFixture) startSession(t *testing.T, sessionID, projectID string) (rootPID int, launch *service.Launch) {
	t.Helper()
	rootPID = fx.spawn(fx.hostPID, 10)
	secret, launch, err := fx.r.launches.Begin(service.Identity{
		Kind:         service.IdentityKindProjectSession,
		Name:         sessionID,
		ProjectID:    projectID,
		ParentLaunch: config.RelaySessionsServiceID,
	})
	assertNoErr(t, err, "Begin session %s", sessionID)
	if _, err := fx.r.launches.Bind(sessionID, secret, peertoken.ForProcessForTest(int32(rootPID), 1)); err != nil {
		t.Fatalf("Bind session %s: %v", sessionID, err)
	}
	return rootPID, launch
}

// rootExits fires the kqueue callback R-S1 registered at Bind, the way a
// real NOTE_EXIT would, and removes the process from the tree.
func (fx *sessionFixture) rootExits(pid int) {
	fx.mu.Lock()
	fire := fx.onExit[pid]
	fx.mu.Unlock()
	fx.procs.remove(pid)
	if fire != nil {
		fire()
	}
}

// conn is one accepted connection from pid: the peer token and the
// membership cache the bridge server builds at accept, and nothing else. A
// test that wants a second connection from the same process calls it twice,
// which is what proves the cache is per-connection.
func (fx *sessionFixture) conn(pid int) context.Context {
	peer := peertoken.ForProcessForTest(int32(pid), 1)
	ctx := bridge.WithCallerPeer(context.Background(), peer)
	return bridge.WithConnMembership(ctx, bridge.NewConnMembership(fx.r, peer, fx.acceptedAt))
}

func twoProjectSettings(t *testing.T) *config.Settings {
	t.Helper()
	s := makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil)
	s.Projects = append(s.Projects, config.Project{
		ID:            "project-b",
		Name:          "B",
		Path:          "/tmp/project-b",
		AllowedMcpIDs: []string{},
		Token:         config.NewSecret("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		TokenHash:     config.HashToken("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	})
	return s
}

func assertUnauthorized(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a refusal, got none", what)
	}
	var coded *jsonrpc.CodedError
	if !errors.As(err, &coded) || coded.RPCCode != jsonrpc.CodeUnauthorized {
		t.Fatalf("%s: err = %v, want CodeUnauthorized", what, err)
	}
}

// The SH §8.3 ancestry matrix, driven through the REAL resolveAuth entry
// point rather than membership.Resolve directly: the walk being correct and
// the walk being wired are different claims, and only the second one is what
// this unit adds.
func TestMembershipAuth_ResolveAuthAncestryMatrix(t *testing.T) {
	fx := newSessionFixture(t, makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil), mcpbroker.NewManager(nil))
	rootPID, _ := fx.startSession(t, "sess-a", "test-project")

	child := fx.spawn(rootPID, 20)
	grandchild := fx.spawn(child, 30)

	// Reparented to launchd: the shell that was its link to the session is
	// gone, and ppid 1 is where the walk stops.
	orphan := fx.spawn(1, 20)

	// A pid recycled after its "child" already existed: the ancestor claims
	// a start time LATER than the process pointing at it, which no genuine
	// parent can.
	recycled := fx.spawn(rootPID, 40)
	afterRecycle := fx.spawn(recycled, 30)

	// A peer that did not exist when its own connection was accepted.
	tooNew := fx.spawn(rootPID, 2000)

	// A chain that climbs past the session entirely into relay's own tree.
	unrelated := fx.spawn(fx.hostPID, 20)

	for _, tc := range []struct {
		name   string
		pid    int
		member bool
	}{
		{"the session's own root process", rootPID, true},
		{"a direct child", child, true},
		{"a grandchild", grandchild, true},
		{"a reparented orphan", orphan, false},
		{"a descendant of a recycled pid", afterRecycle, false},
		{"a peer younger than its connection", tooNew, false},
		{"a process relay launched itself", unrelated, false},
		{"a pid relay cannot read", unknownPID, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth, err := fx.r.resolveAuth(fx.conn(tc.pid), "", service.OpProjectTools)
			if !tc.member {
				assertUnauthorized(t, err, tc.name)
				return
			}
			if err != nil {
				t.Fatalf("%s: resolveAuth: %v", tc.name, err)
			}
			if auth.stored.ProjectID != "test-project" || auth.sessionID != "sess-a" {
				t.Fatalf("%s: resolved to project %q session %q", tc.name, auth.stored.ProjectID, auth.sessionID)
			}
		})
	}
}

// A member's scope is the project's own, byte for byte what the project's
// token resolves to: a session identifies a caller, it never widens one.
func TestMembershipAuth_MemberScopeEqualsTokenScope(t *testing.T) {
	s := makeSettings(
		map[string]config.Permission{"fsmcp": config.PermOn, "macmcp": config.PermOff},
		map[string][]string{"fsmcp": {"write_file"}},
		map[string]json.RawMessage{"fsmcp": json.RawMessage(`{"allowed_dirs":["/x"]}`)},
	)
	fx := newSessionFixture(t, s, mcpbroker.NewManager(nil))
	rootPID, _ := fx.startSession(t, "sess-a", "test-project")

	byToken, err := fx.r.resolveAuth(context.Background(), testToken, service.OpProjectTools)
	assertNoErr(t, err, "token auth")
	byMember, err := fx.r.resolveAuth(fx.conn(fx.spawn(rootPID, 20)), "", service.OpProjectTools)
	assertNoErr(t, err, "member auth")

	want, _ := json.Marshal(byToken.stored)
	got, _ := json.Marshal(byMember.stored)
	if string(want) != string(got) {
		t.Errorf("scope differs between auth paths:\n token: %s\nmember: %s", want, got)
	}
	if byToken.sessionID != "" {
		t.Errorf("a bearer resolved with session id %q", byToken.sessionID)
	}
}

// Project B's member reaching project A gets B's grant and B's refusals —
// never A's, and never a blend.
func TestMembershipAuth_ProjectBMemberIsRefusedProjectA(t *testing.T) {
	var reached atomic.Int32
	mgr := mcpbroker.NewManager(nil)
	addMockConn(mgr, "fsmcp", newMockConn("fsmcp", simpleTools("read_file"),
		func(context.Context, string, interface{}) (json.RawMessage, error) {
			reached.Add(1)
			return json.RawMessage(`{"ok":true}`), nil
		}))
	fx := newSessionFixture(t, twoProjectSettings(t), mgr)

	rootA, _ := fx.startSession(t, "sess-a", "test-project")
	rootB, _ := fx.startSession(t, "sess-b", "project-b")
	memberA := fx.spawn(rootA, 20)
	memberB := fx.spawn(rootB, 20)

	// A's member reaches fsmcp, which only A is granted.
	if _, err := fx.r.CallTool(fx.conn(memberA), "read_file", json.RawMessage(`{}`), ""); err != nil {
		t.Fatalf("A's member calling A's tool: %v", err)
	}
	callsAfterA := reached.Load()

	authB, err := fx.r.resolveAuth(fx.conn(memberB), "", service.OpProjectTools)
	assertNoErr(t, err, "B's member auth")
	if authB.stored.ProjectID != "project-b" {
		t.Fatalf("B's member resolved to project %q", authB.stored.ProjectID)
	}
	if _, err := fx.r.CallTool(fx.conn(memberB), "read_file", json.RawMessage(`{}`), ""); err == nil {
		t.Fatal("B's member reached a tool only project A is granted")
	}
	if reached.Load() != callsAfterA {
		t.Fatal("a refused cross-project call still reached the MCP")
	}
}

// A service's launch identity and a project_session's are different
// principals with no overlap: a service reaches no project operation, and is
// refused AT step 2 rather than being allowed to fall through to the
// membership walk (which its own ancestry might otherwise satisfy).
func TestMembershipAuth_ServiceIdentityIsRefusedProjectOps(t *testing.T) {
	fx := newSessionFixture(t, makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil), mcpbroker.NewManager(nil))
	rootPID, _ := fx.startSession(t, "sess-a", "test-project")

	// The service process is a real descendant of a live session root, so
	// step 3 WOULD admit it. Step 2 must answer first and refuse.
	svcPID := fx.spawn(rootPID, 20)
	svcPeer := peertoken.ForProcessForTest(int32(svcPID), 1)
	secret, _, err := fx.r.launches.Begin(service.Identity{Kind: service.IdentityKindService, Name: "svc", Capabilities: capsBridge})
	assertNoErr(t, err, "Begin service")
	_, err = fx.r.launches.Bind("svc", secret, svcPeer)
	assertNoErr(t, err, "Bind service")

	ctx := bridge.WithConnMembership(
		bridge.WithCallerPeer(context.Background(), svcPeer),
		bridge.NewConnMembership(fx.r, svcPeer, fx.acceptedAt))

	for _, op := range []service.Operation{service.OpProjectTools, service.OpProjectDescribe, service.OpProjectListSkills} {
		_, err := fx.r.resolveAuth(ctx, "", op)
		assertUnauthorized(t, err, "service identity attempting "+string(op))
	}
	if _, err := fx.r.DescribeProject(ctx, ""); err == nil {
		t.Fatal("a service identity described a project")
	}
}

// A bad token is a bad token: it is never re-read as "no token" and never
// reaches step 2 or step 3, however impeccable the caller's ancestry.
func TestMembershipAuth_AnInvalidTokenNeverFallsThrough(t *testing.T) {
	fx := newSessionFixture(t, makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil), mcpbroker.NewManager(nil))
	rootPID, _ := fx.startSession(t, "sess-a", "test-project")
	ctx := fx.conn(fx.spawn(rootPID, 20))

	// Same connection, same member: tokenless succeeds, so the refusal below
	// can only be the token's doing.
	if _, err := fx.r.resolveAuth(ctx, "", service.OpProjectTools); err != nil {
		t.Fatalf("tokenless member: %v", err)
	}
	for _, bad := range []string{"not-a-token", testToken + "x", " " + testToken} {
		_, err := fx.r.resolveAuth(ctx, bad, service.OpProjectTools)
		assertUnauthorized(t, err, "member presenting an invalid token")
		if !errors.Is(err, config.ErrInvalidToken) {
			t.Errorf("token %q: err = %v, want ErrInvalidToken — a presented credential must be judged as one", bad, err)
		}
	}
}

// Nothing a caller asserts about its own working directory reaches
// authorization: the field is dropped at the transport, and a non-member
// naming a real project's path is refused exactly like one naming nothing.
func TestMembershipAuth_AClientAssertedCwdIsIgnored(t *testing.T) {
	s := makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil)
	s.Projects[0].Path = t.TempDir()
	fx := newSessionFixture(t, s, mcpbroker.NewManager(nil))

	// A process that is nobody's descendant, claiming to sit in the project.
	outsider := fx.conn(fx.spawn(fx.hostPID, 20))
	_, err := fx.r.resolveAuth(outsider, "", service.OpProjectTools)
	assertUnauthorized(t, err, "a non-member inside the project directory")
	if !errors.Is(err, config.ErrNoToken) {
		t.Errorf("err = %v, want ErrNoToken: a tokenless non-member has presented nothing at all", err)
	}
}

// C3's cache rule, both halves: the walk runs once per connection, and every
// request re-checks that the session it found is still live.
func TestMembershipAuth_AnEndedSessionRefusesItsCachedConnection(t *testing.T) {
	fx := newSessionFixture(t, makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil), mcpbroker.NewManager(nil))
	rootPID, launch := fx.startSession(t, "sess-a", "test-project")
	ctx := fx.conn(fx.spawn(rootPID, 20))

	if _, err := fx.r.resolveAuth(ctx, "", service.OpProjectTools); err != nil {
		t.Fatalf("first request on the connection: %v", err)
	}

	launch.End()

	// The very next request on the SAME connection, with the answer already
	// cached, must be refused — no stale-valid window of any width.
	_, err := fx.r.resolveAuth(ctx, "", service.OpProjectTools)
	assertUnauthorized(t, err, "request after the session ended")

	// And a session that starts again under the same id does not revive the
	// connection: the walk is not re-run, and the cache was cleared.
	fx.startSession(t, "sess-a", "test-project")
	_, err = fx.r.resolveAuth(ctx, "", service.OpProjectTools)
	assertUnauthorized(t, err, "request after a same-named session restarted")
}

// A session id is reused by design: C5's resume re-launches a dormant
// session under the SAME id with a new root process. A connection admitted
// under the old root must not be inherited by the new one — its peer was
// never proved to descend from that process, and the id matching is a
// coincidence of the resume contract, not evidence.
//
// The order matters: the successor binds with NO refused request in between,
// so the cache still holds the old session and the liveness re-check is the
// only thing standing between that connection and the new session's grant.
func TestMembershipAuth_ASameNamedSuccessorDoesNotInheritTheConnection(t *testing.T) {
	fx := newSessionFixture(t, makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil), mcpbroker.NewManager(nil))
	rootPID, _ := fx.startSession(t, "sess-a", "test-project")
	ctx := fx.conn(fx.spawn(rootPID, 20))

	if _, err := fx.r.resolveAuth(ctx, "", service.OpProjectTools); err != nil {
		t.Fatalf("under the original session: %v", err)
	}

	// Begin replaces the live launch of the same name, exactly as a resume
	// would: same session id, a different root process.
	successorPID, _ := fx.startSession(t, "sess-a", "test-project")
	if successorPID == rootPID {
		t.Fatal("the fixture reused the root pid; this test proves nothing")
	}
	if _, ok := fx.r.launches.Bound("sess-a"); !ok {
		t.Fatal("the successor session is not bound; this test proves nothing")
	}

	_, err := fx.r.resolveAuth(ctx, "", service.OpProjectTools)
	assertUnauthorized(t, err, "the old connection under a successor session of the same id")
}

// The other half of C3's cache rule: the ancestry walk runs ONCE per
// connection, whatever it answered, and a second connection from the same
// process is a second question. A walk per request would put a syscall
// chain on every tool call and hand any caller a cheap way to make relay do
// unbounded work.
func TestMembershipAuth_TheWalkRunsOncePerConnection(t *testing.T) {
	fx := newSessionFixture(t, makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil), mcpbroker.NewManager(nil))
	rootPID, _ := fx.startSession(t, "sess-a", "test-project")
	memberPID := fx.spawn(rootPID, 20)
	outsiderPID := fx.spawn(fixtureHostPID, 20)

	for _, tc := range []struct {
		name string
		pid  int
	}{
		{"a member", memberPID},
		{"a non-member", outsiderPID},
	} {
		ctx := fx.conn(tc.pid)
		before := fx.procs.readCount()
		for i := 0; i < 5; i++ {
			_, _ = fx.r.resolveAuth(ctx, "", service.OpProjectTools)
		}
		first := fx.procs.readCount() - before

		ctx2 := fx.conn(tc.pid)
		_, _ = fx.r.resolveAuth(ctx2, "", service.OpProjectTools)
		second := fx.procs.readCount() - before - first

		if second == 0 {
			t.Fatalf("%s: a NEW connection reused the previous one's answer", tc.name)
		}
		if first > second {
			t.Fatalf("%s: five requests read the process table %d times but one request read it %d; the walk is not cached per connection", tc.name, first, second)
		}
	}
}

// The root process exiting ends the session through the kqueue watch R-S1
// registered at Bind, with the same next-request refusal — this is L13's
// hermetic half (a surviving descendant loses access the moment its root
// dies).
func TestMembershipAuth_RootExitRefusesSurvivingDescendants(t *testing.T) {
	fx := newSessionFixture(t, makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil), mcpbroker.NewManager(nil))
	rootPID, _ := fx.startSession(t, "sess-a", "test-project")
	ctx := fx.conn(fx.spawn(rootPID, 20))

	if _, err := fx.r.resolveAuth(ctx, "", service.OpProjectTools); err != nil {
		t.Fatalf("while the root was live: %v", err)
	}
	fx.rootExits(rootPID)
	_, err := fx.r.resolveAuth(ctx, "", service.OpProjectTools)
	assertUnauthorized(t, err, "descendant of an exited root")

	// A connection opened afterwards is refused by the walk itself, not by
	// the cache — the root is no longer in the table to match.
	_, err = fx.r.resolveAuth(fx.conn(fx.spawn(rootPID, 20)), "", service.OpProjectTools)
	assertUnauthorized(t, err, "new connection after the root exited")
}

// The exit callback and an explicit End race in production (a kqueue
// NOTE_EXIT and a host-driven End can land together), and membership.WatchExit
// documents that cancel() may return while onExit is still running. Ending a
// session must therefore be idempotent under repetition and under
// concurrency, and must leave the same refusal behind either way.
func TestMembershipAuth_SessionEndIsIdempotentUnderConcurrentExit(t *testing.T) {
	fx := newSessionFixture(t, makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil), mcpbroker.NewManager(nil))
	rootPID, launch := fx.startSession(t, "sess-a", "test-project")
	ctx := fx.conn(fx.spawn(rootPID, 20))
	if _, err := fx.r.resolveAuth(ctx, "", service.OpProjectTools); err != nil {
		t.Fatalf("before the exit: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				fx.rootExits(rootPID) // the kqueue callback
			case 1:
				launch.End() // an explicit end racing it
			case 2:
				fx.r.launches.EndByProject("test-project")
			default:
				// Requests in flight across the whole window: each must
				// answer, and none may panic or observe a half-ended table.
				_, _ = fx.r.resolveAuth(ctx, "", service.OpProjectTools)
			}
		}(i)
	}
	wg.Wait()

	_, err := fx.r.resolveAuth(ctx, "", service.OpProjectTools)
	assertUnauthorized(t, err, "after concurrent ends settled")
	if _, ok := fx.r.launches.Bound("sess-a"); ok {
		t.Fatal("the session is still bound after every end path ran")
	}
}

// The binding amendment: WatchExit returning ErrExited at registration means
// the root is ALREADY gone. The Hello is refused (there is no live process to
// bind), and the launch is ended rather than left sitting unbound with a
// spendable secret until its TTL expires.
func TestMembershipAuth_WatchExitErrExitedEndsTheSessionImmediately(t *testing.T) {
	fx := newSessionFixture(t, makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil), mcpbroker.NewManager(nil))

	rootPID := fx.spawn(fx.hostPID, 10)
	secret, _, err := fx.r.launches.Begin(service.Identity{
		Kind: service.IdentityKindProjectSession, Name: "sess-dead", ProjectID: "test-project",
		ParentLaunch: config.RelaySessionsServiceID,
	})
	assertNoErr(t, err, "Begin")

	// The root dies between relay reading its start time and the watch
	// landing: the fixture's watcher reports exactly what a real kqueue
	// registration against a dead pid reports.
	fx.r.launches.SetRootWatcherForTest(func(int, membership.ProcInfo, func()) (func(), error) {
		return nil, membership.ErrExited
	})

	if _, err := fx.r.launches.Bind("sess-dead", secret, peertoken.ForProcessForTest(int32(rootPID), 1)); err == nil {
		t.Fatal("Hello succeeded with an already-exited root")
	}
	if _, ok := fx.r.launches.Bound("sess-dead"); ok {
		t.Fatal("the launch bound despite the refusal")
	}

	// The launch is gone, not merely unbound: a second Hello presenting the
	// same secret has nothing left to bind.
	if _, err := fx.r.launches.Bind("sess-dead", secret, peertoken.ForProcessForTest(int32(rootPID), 1)); err == nil {
		t.Fatal("the secret was still spendable after ErrExited: the launch outlived its root")
	}
	if fx.r.launches.Len() != 0 {
		t.Fatalf("launch table holds %d launches; the ended one is still there", fx.r.launches.Len())
	}
}

// DescribeProject accepts a project bearer or a session (C3's note, and C1's
// project_session row giving root and member the same operations), and
// nobody else.
func TestMembershipAuth_DescribeProjectAcceptsATokenOrASession(t *testing.T) {
	fx := newSessionFixture(t, makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil), mcpbroker.NewManager(nil))
	rootPID, _ := fx.startSession(t, "sess-a", "test-project")

	byToken, err := fx.r.DescribeProject(context.Background(), testToken)
	assertNoErr(t, err, "DescribeProject by token")

	for name, ctx := range map[string]context.Context{
		"the session's root process": fx.conn(rootPID),
		"a member":                   fx.conn(fx.spawn(rootPID, 20)),
	} {
		got, err := fx.r.DescribeProject(ctx, "")
		if err != nil {
			t.Fatalf("DescribeProject by %s: %v", name, err)
		}
		if got.ID != byToken.ID || got.Name != byToken.Name {
			t.Errorf("%s described %+v; want the same project the token describes (%+v)", name, got, byToken)
		}
	}

	if _, err := fx.r.DescribeProject(fx.conn(fx.spawn(1, 20)), ""); err == nil {
		t.Fatal("a non-member described a project")
	}
	if _, err := fx.r.DescribeProject(context.Background(), "not-a-token"); err == nil {
		t.Fatal("an invalid token described a project")
	}
}

// ListSkillBuckets is the third project operation a session reaches, and it
// must see exactly the surface ListTools does — a skill file written for a
// session must not name a tool the session cannot call.
func TestMembershipAuth_ListSkillsMatchesTheTokenSurface(t *testing.T) {
	mgr := mcpbroker.NewManager(nil)
	addMockConn(mgr, "fsmcp", newMockConn("fsmcp", simpleTools("read_file", "write_file"), nil))
	s := makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, map[string][]string{"fsmcp": {"write_file"}}, nil)
	fx := newSessionFixture(t, s, mgr)
	rootPID, _ := fx.startSession(t, "sess-a", "test-project")
	ctx := fx.conn(fx.spawn(rootPID, 20))

	byToken, err := fx.r.ListSkillBuckets(context.Background(), testToken)
	assertNoErr(t, err, "ListSkillBuckets by token")
	bySession, err := fx.r.ListSkillBuckets(ctx, "")
	assertNoErr(t, err, "ListSkillBuckets by session")

	want, _ := json.Marshal(byToken)
	got, _ := json.Marshal(bySession)
	if string(want) != string(got) {
		t.Errorf("skill surface differs:\n token: %s\nsession: %s", want, got)
	}
	tools, err := fx.r.ListTools(ctx, "")
	assertNoErr(t, err, "ListTools by session")
	if names := unmarshalTools(t, tools); len(names) != 1 || names[0].Name != "read_file" {
		t.Errorf("session ListTools = %+v; want only read_file", names)
	}
}
