// Package server exposes the core as bounded HTTP/JSON over Unix sockets and
// provides the matching client. There is no TCP listener.
package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/core"
)

const (
	maxBody     = 1 << 20
	maxInFlight = 32
	opTimeout   = 20 * time.Second
	// RoleHeader lets a client narrow, never widen, its socket's roles.
	RoleHeader = "X-Ichnora-Role"
)

type peerKey struct{}

type peer struct {
	pid, uid int
	ok       bool
}

// Server serves one core on several sockets with per-socket roles.
type Server struct {
	core    *core.Core
	log     *slog.Logger
	mu      sync.Mutex
	servers []*http.Server
	sem     chan struct{}
}

// New creates a server.
func New(c *core.Core, log *slog.Logger) *Server {
	return &Server{core: c, log: log, sem: make(chan struct{}, maxInFlight)}
}

// Listen creates a private Unix socket, replacing only a stale socket file.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("socket path exists and is not a socket")
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, errors.New("another service is listening on the socket")
		}
		os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Serve handles requests on ln with the given roles until Shutdown.
func (s *Server) Serve(ln net.Listener, roles []contract.Role) error {
	granted := map[contract.Role]bool{}
	for _, r := range roles {
		granted[r] = true
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handle(w, r, granted) }),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      opTimeout + 10*time.Second,
		MaxHeaderBytes:    16 << 10,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, peerKey{}, peerOf(c))
		},
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	s.mu.Lock()
	s.servers = append(s.servers, srv)
	s.mu.Unlock()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops all listeners and waits for in-flight requests.
func (s *Server) Shutdown(ctx context.Context) {
	s.mu.Lock()
	servers := append([]*http.Server(nil), s.servers...)
	s.mu.Unlock()
	for _, srv := range servers {
		_ = srv.Shutdown(ctx)
	}
}

func peerOf(c net.Conn) peer {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return peer{}
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return peer{}
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || credErr != nil {
		return peer{}
	}
	return peer{pid: int(cred.Pid), uid: int(cred.Uid), ok: true}
}

// callerFor derives roles and the verified working directory of the peer.
func callerFor(r *http.Request, granted map[contract.Role]bool) (core.Caller, bool) {
	p, _ := r.Context().Value(peerKey{}).(peer)
	if !p.ok || p.uid != os.Getuid() {
		return core.Caller{}, false
	}
	roles := map[contract.Role]bool{}
	for r := range granted {
		roles[r] = true
	}
	if req := r.Header.Get(RoleHeader); req != "" {
		narrowed := map[contract.Role]bool{}
		for _, name := range strings.Split(req, ",") {
			role := contract.Role(strings.TrimSpace(name))
			if granted[role] {
				narrowed[role] = true
			}
		}
		roles = narrowed
	}
	cwd, err := os.Readlink("/proc/" + strconv.Itoa(p.pid) + "/cwd")
	if err != nil {
		cwd = ""
	}
	return core.Caller{Roles: roles, Cwd: cwd}, true
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request, granted map[contract.Role]bool) {
	op := strings.TrimPrefix(r.URL.Path, "/v1/")
	w.Header().Set("Content-Type", "application/json")
	fail := func(status int, e *contract.Error) {
		w.WriteHeader(status)
		_, _ = w.Write(contract.EncodeError(op, e))
	}
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/") || len(op) > 32 {
		fail(http.StatusNotFound, contract.Errf(contract.CodeInvalid, "unknown operation"))
		return
	}
	caller, ok := callerFor(r, granted)
	if !ok {
		fail(http.StatusForbidden, contract.Errf(contract.CodeForbidden, "peer credentials are missing or belong to another user"))
		return
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		fail(http.StatusServiceUnavailable, contract.Errf(contract.CodeBusy, "too many requests in flight; retry later"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	body := http.MaxBytesReader(w, r.Body, maxBody)
	out, perr := Dispatch(ctx, s.core, caller, op, body)
	if perr != nil {
		fail(http.StatusOK, perr)
		return
	}
	_, _ = w.Write(out)
}

// Dispatch decodes one request, runs the operation and encodes the bounded
// response. It is shared by the socket server and in-process tests.
func Dispatch(ctx context.Context, c *core.Core, caller core.Caller, op string, body io.Reader) ([]byte, *contract.Error) {
	fail := func(err error) ([]byte, *contract.Error) { return nil, c.AsProtocolError(op, err) }
	switch op {
	case "search":
		var req contract.SearchRequest
		if err := contract.Decode(body, &req); err != nil {
			return fail(err)
		}
		res, budget, err := c.Search(ctx, caller, req)
		if err != nil {
			return fail(err)
		}
		return encode(op, res, budget, c)
	case "read":
		var req contract.ReadRequest
		if err := contract.Decode(body, &req); err != nil {
			return fail(err)
		}
		res, budget, err := c.Read(ctx, caller, req)
		if err != nil {
			return fail(err)
		}
		return encode(op, res, budget, c)
	case "evidence":
		var req contract.EvidenceRequest
		if err := contract.Decode(body, &req); err != nil {
			return fail(err)
		}
		res, budget, err := c.Evidence(ctx, caller, req)
		if err != nil {
			return fail(err)
		}
		return encode(op, res, budget, c)
	case "resume":
		var req contract.ResumeRequest
		if err := contract.Decode(body, &req); err != nil {
			return fail(err)
		}
		res, budget, err := c.Resume(ctx, caller, req)
		if err != nil {
			return fail(err)
		}
		return encode(op, res, budget, c)
	case "status":
		var req contract.StatusRequest
		if err := contract.Decode(body, &req); err != nil {
			return fail(err)
		}
		res, budget, err := c.Status(ctx, caller, req)
		if err != nil {
			return fail(err)
		}
		return encode(op, res, budget, c)
	case "capture":
		var req contract.CaptureRequest
		if err := contract.Decode(body, &req); err != nil {
			return fail(err)
		}
		return result(op, c, func() (any, error) { return c.Capture(ctx, caller, req) })
	case "update":
		var req contract.UpdateRequest
		if err := contract.Decode(body, &req); err != nil {
			return fail(err)
		}
		return result(op, c, func() (any, error) { return c.Update(ctx, caller, req) })
	case "task":
		var req contract.TaskRequest
		if err := contract.Decode(body, &req); err != nil {
			return fail(err)
		}
		return result(op, c, func() (any, error) { return c.Task(ctx, caller, req) })
	case "hook":
		var req contract.HookRequest
		if err := contract.Decode(body, &req); err != nil {
			return fail(err)
		}
		return result(op, c, func() (any, error) { return c.Hook(ctx, caller, req) })
	}
	admin := map[string]func(contract.AdminRequest) (any, error){
		"enroll":            func(r contract.AdminRequest) (any, error) { return c.Enroll(ctx, caller, r) },
		"withdraw":          func(r contract.AdminRequest) (any, error) { return c.Withdraw(ctx, caller, r) },
		"projects":          func(contract.AdminRequest) (any, error) { return c.Projects(ctx, caller) },
		"discover":          func(contract.AdminRequest) (any, error) { return c.Discover(ctx, caller) },
		"policy":            func(r contract.AdminRequest) (any, error) { return c.Policy(ctx, caller, r) },
		"share":             func(r contract.AdminRequest) (any, error) { return c.ShareClaim(ctx, caller, r) },
		"revoke":            func(r contract.AdminRequest) (any, error) { return c.Revoke(ctx, caller, r) },
		"retention_preview": func(r contract.AdminRequest) (any, error) { return c.RetentionPreview(ctx, caller, r) },
		"retention_apply":   func(r contract.AdminRequest) (any, error) { return c.RetentionApply(ctx, caller, r) },
		"import_preview":    func(r contract.AdminRequest) (any, error) { return c.Import(ctx, caller, r, false) },
		"import_apply":      func(r contract.AdminRequest) (any, error) { return c.Import(ctx, caller, r, true) },
		"reconcile":         func(contract.AdminRequest) (any, error) { return c.Reconcile(ctx, caller) },
		"backup":            func(r contract.AdminRequest) (any, error) { return c.Backup(ctx, caller, r) },
		"environment":       func(contract.AdminRequest) (any, error) { return c.EnvironmentStatus(ctx, caller) },
	}
	fn, ok := admin[op]
	if !ok {
		return nil, contract.Errf(contract.CodeInvalid, "unknown operation")
	}
	var req contract.AdminRequest
	if err := contract.Decode(body, &req); err != nil {
		return fail(err)
	}
	return result(op, c, func() (any, error) { return fn(req) })
}

func encode(op string, res any, budget int, c *core.Core) ([]byte, *contract.Error) {
	out, err := contract.Encode(op, res, budget)
	if err != nil {
		return nil, c.AsProtocolError(op, err)
	}
	return out, nil
}

func result(op string, c *core.Core, fn func() (any, error)) ([]byte, *contract.Error) {
	res, err := fn()
	if err != nil {
		return nil, c.AsProtocolError(op, err)
	}
	return encode(op, res, 0, c)
}
