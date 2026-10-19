package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timfewi/ichnora/internal/config"
	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/ingest"
)

// testEnv runs the real service on temporary sockets and drives the real CLI.
// The service resolves scope from this process's working directory, so
// cli() changes directory before each call; tests in this package are serial.
type testEnv struct {
	t                     *testing.T
	root, ws, state       string
	alpha, beta           string
	claudeRoot, codexRoot string
	cfg                   config.Config
	cfgPath               string
	cancel                context.CancelFunc
	done                  chan error
	logs                  *syncBuffer
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mkRepo creates a minimal Git layout that gitHead can read without git.
func mkRepo(t *testing.T, dir, head string) {
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(dir, ".git", "refs", "heads", "main"), head+"\n")
}

func newEnv(t *testing.T, mutate func(*config.Config)) *testEnv {
	t.Helper()
	root, err := os.MkdirTemp("", "ich")
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	e := &testEnv{t: t, root: root, ws: filepath.Join(root, "code"), state: filepath.Join(root, "state"),
		claudeRoot: filepath.Join(root, "claude"), codexRoot: filepath.Join(root, "codex"), logs: &syncBuffer{}}
	e.alpha, e.beta = filepath.Join(e.ws, "alpha"), filepath.Join(e.ws, "beta")
	mkRepo(t, e.alpha, strings.Repeat("a", 40))
	mkRepo(t, e.beta, strings.Repeat("b", 40))
	writeFile(t, filepath.Join(e.alpha, "parser", "cache.go"), "package parser\n\n// Cache keys include the source digest.\nfunc Key() string { return \"v1\" }\n")
	writeFile(t, filepath.Join(e.beta, "README.md"), "beta project\n")
	for _, d := range []string{e.claudeRoot, e.codexRoot} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.cfg = config.Config{StateDir: e.state, AgentSocket: filepath.Join(root, "run", "a.sock"),
		AdminSocket: filepath.Join(root, "run", "b.sock"), WorkspaceRoots: []string{e.ws},
		SessionSources: map[string][]string{"claude": {e.claudeRoot}, "codex": {e.codexRoot}}}
	if mutate != nil {
		mutate(&e.cfg)
	}
	if e.cfg, err = e.cfg.WithDefaults(); err != nil {
		t.Fatal(err)
	}
	e.cfgPath = filepath.Join(root, "config.json")
	data, _ := json.Marshal(e.cfg)
	writeFile(t, e.cfgPath, string(data))
	for k, v := range map[string]string{"ICHNORA_SOCKET": e.cfg.AgentSocket, "ICHNORA_ADMIN_SOCKET": e.cfg.AdminSocket,
		"ICHNORA_CONFIG": e.cfgPath, "CLAUDE_CODE_SESSION_ID": "", "CODEX_THREAD_ID": "", "OPENCODE_SESSION_ID": ""} {
		t.Setenv(k, v)
	}
	wd, _ := os.Getwd()
	t.Cleanup(func() {
		e.stop()
		os.Chdir(wd)
		os.RemoveAll(root)
	})
	e.start()
	return e
}

func (e *testEnv) start() {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel, e.done = cancel, make(chan error, 1)
	ready := make(chan struct{})
	log := slog.New(slog.NewJSONHandler(e.logs, nil))
	go func() { e.done <- runService(ctx, e.cfg, log, ready) }()
	select {
	case <-ready:
	case err := <-e.done:
		cancel()
		e.cancel = nil
		e.t.Fatalf("service failed to start: %v", err)
	case <-time.After(10 * time.Second):
		e.t.Fatal("service did not start")
	}
}

func (e *testEnv) stop() {
	if e.cancel != nil {
		e.cancel()
		<-e.done
		e.cancel = nil
	}
}

// result is the decoded contract envelope.
type result struct {
	Raw    string
	Code   int
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *contract.Error `json:"error"`
}

func (r result) into(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Result, v); err != nil {
		t.Fatalf("decode result: %v\n%s", err, r.Raw)
	}
}

func (r result) errCode() contract.Code {
	if r.Error == nil {
		return ""
	}
	return r.Error.Code
}

// cli runs the real command line in dir with optional stdin.
func (e *testEnv) cli(dir, stdin string, args ...string) result {
	e.t.Helper()
	if err := os.Chdir(dir); err != nil {
		e.t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb)
	r := result{Raw: strings.TrimSpace(out.String()), Code: code}
	if r.Raw != "" && strings.HasPrefix(r.Raw, "{") {
		if err := json.Unmarshal([]byte(r.Raw), &r); err != nil {
			e.t.Fatalf("CLI output is not valid JSON: %v\n%s", err, r.Raw)
		}
	}
	return r
}

// must fails the test unless the command succeeded.
func (e *testEnv) must(dir, stdin string, args ...string) result {
	e.t.Helper()
	r := e.cli(dir, stdin, args...)
	if r.Code != 0 || !r.OK {
		e.t.Fatalf("ichnora %s failed (exit %d): %s", strings.Join(args, " "), r.Code, r.Raw)
	}
	return r
}

func (e *testEnv) enroll(path string, extra ...string) string {
	e.t.Helper()
	r := e.must(e.root, "", append([]string{"enroll", path}, extra...)...)
	var p struct {
		ID string `json:"id"`
	}
	r.into(e.t, &p)
	return p.ID
}

// fixture writes a synthetic native transcript with placeholders replaced.
func (e *testEnv) fixture(name, dest, session, cwd string) string {
	e.t.Helper()
	src, err := os.ReadFile(filepath.Join(testdataAbs, name))
	if err != nil {
		e.t.Fatal(err)
	}
	text := strings.NewReplacer("__SESSION__", session, "__CWD__", cwd).Replace(string(src))
	writeFile(e.t, dest, text)
	return dest
}

// testdataAbs is resolved before any test changes directory.
var testdataAbs string

func init() {
	wd, _ := os.Getwd()
	testdataAbs = filepath.Join(wd, "testdata")
}

// hook sends a native hook payload through `ichnora hook HARNESS` from dir.
func (e *testEnv) hook(dir, harness string, payload map[string]any) string {
	e.t.Helper()
	data, _ := json.Marshal(payload)
	if err := os.Chdir(dir); err != nil {
		e.t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"hook", harness}, bytes.NewReader(data), &out, &errb); code != 0 {
		e.t.Fatalf("hook exit %d", code)
	}
	return out.String()
}

func capture(t *testing.T, claims ...contract.ClaimInput) string {
	t.Helper()
	data, _ := json.Marshal(contract.CaptureRequest{Claims: claims})
	return string(data)
}

func (e *testEnv) captureIDs(dir string, claims ...contract.ClaimInput) []string {
	e.t.Helper()
	r := e.must(dir, capture(e.t, claims...), "capture")
	var out contract.CaptureResult
	r.into(e.t, &out)
	if !out.Durable || len(out.IDs) != len(claims) {
		e.t.Fatalf("capture not durable: %s", r.Raw)
	}
	return out.IDs
}

func (e *testEnv) search(dir string, args ...string) contract.SearchResult {
	e.t.Helper()
	r := e.must(dir, "", append([]string{"search"}, args...)...)
	var out contract.SearchResult
	r.into(e.t, &out)
	return out
}

func ids(items []contract.Item) []string {
	var out []string
	for _, i := range items {
		out = append(out, i.ID)
	}
	return out
}

func find(items []contract.Item, id string) *contract.Item {
	for i := range items {
		if items[i].ID == id {
			return &items[i]
		}
	}
	return nil
}

func sha(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return ingest.Digest(b)
}
