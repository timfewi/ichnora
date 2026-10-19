// Command ichnora is the single executable: service, CLI, MCP bridge and
// harness hook adapter.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/timfewi/ichnora/internal/config"
	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/hook"
	"github.com/timfewi/ichnora/internal/mcpbridge"
	"github.com/timfewi/ichnora/internal/server"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "dev"

const usage = `usage: ichnora <command> [flags] [args]

Agent commands (agent socket):
  search QUERY            scoped retrieval (--view, --task, --session, --kind, --file, --since, --until, --limit, --cursor)
  read ID...              batch details
  evidence CLAIM_ID       validate support and dependencies (--lines)
  capture                 record claims; JSON CaptureRequest on stdin
  update ID               --status disputed|invalidated --reason TEXT
  task list|create|checkpoint|transition   (see docs/contract.md)
  resume TASK_ID          working context of an explicitly selected task
  status                  health, scope, storage and adapter coverage
  hook claude|codex|opencode   native hook adapter (payload on stdin)
  mcp                     stdio MCP bridge (--role reader|reader,capture)

Owner commands (admin socket):
  projects | discover | enroll PATH | withdraw PROJECT | policy PATH
  share CLAIM_ID --to PROJECT | revoke SHARE_ID
  retention preview|apply --selector claim|session|project --target ID [--confirm TOKEN]
  import preview|apply --harness H [--confirm TOKEN] PATH...
  reconcile | backup OUT | environment

Service and maintenance:
  serve [--config PATH]   run the service (owns the database)
  restore --config PATH BACKUP   replace the database while the service is stopped
  version

Read commands accept --max-bytes. Output is the contract JSON; exit codes follow docs/contract.md.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// common flags shared by every client command.
type common struct {
	socket, adminSocket, config string
	maxBytes                    int
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.socket, "socket", os.Getenv("ICHNORA_SOCKET"), "agent socket")
	fs.StringVar(&c.adminSocket, "admin-socket", os.Getenv("ICHNORA_ADMIN_SOCKET"), "admin socket")
	fs.StringVar(&c.config, "config", config.DefaultConfigPath(), "service configuration")
	fs.IntVar(&c.maxBytes, "max-bytes", 0, "response byte budget")
}

func (c *common) client(admin bool) (server.Client, error) {
	sock := c.socket
	if admin {
		sock = c.adminSocket
	}
	if sock == "" {
		cfg, err := config.Load(c.config)
		if err != nil {
			return server.Client{}, err
		}
		sock = cfg.AgentSocket
		if admin {
			sock = cfg.AdminSocket
		}
	}
	return server.Client{Socket: sock}, nil
}

// sessionHint reads the harness session from the environment. It is an
// attribution hint only; the service never derives scope from it.
func sessionHint() *contract.SessionHint {
	for _, e := range []struct{ env, harness string }{
		{"CLAUDE_CODE_SESSION_ID", "claude"}, {"CODEX_THREAD_ID", "codex"}, {"OPENCODE_SESSION_ID", "opencode"},
	} {
		if v := os.Getenv(e.env); v != "" && len(v) <= 200 {
			return &contract.SessionHint{Harness: e.harness, NativeID: v}
		}
	}
	return nil
}

// list collects a repeatable string flag.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	ctx := context.Background()
	var c common
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	c.register(fs)
	// interspersed parses flags after positional arguments as well.
	parse := func() ([]string, bool) {
		var pos []string
		for {
			if err := fs.Parse(rest); err != nil {
				return nil, false
			}
			rest = fs.Args()
			if len(rest) == 0 {
				return pos, true
			}
			pos = append(pos, rest[0])
			rest = rest[1:]
		}
	}
	call := func(admin bool, op string, req any) int {
		cl, err := c.client(admin)
		if err != nil {
			fmt.Fprintln(stderr, "ichnora:", err)
			return 2
		}
		out, perr := cl.Call(ctx, op, req)
		if out == nil && perr != nil {
			out = contract.EncodeError(op, perr)
		}
		fmt.Fprintln(stdout, string(out))
		if perr != nil {
			return contract.ExitCode(perr.Code)
		}
		return 0
	}
	switch cmd {
	case "version":
		fmt.Fprintln(stdout, version, contract.Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "serve":
		if _, ok := parse(); !ok {
			return 2
		}
		return serve(c.config, stderr)
	case "restore":
		pos, ok := parse()
		if !ok || len(pos) != 1 {
			fmt.Fprintln(stderr, "usage: ichnora restore --config PATH BACKUP")
			return 2
		}
		return restore(c.config, pos[0], stdout, stderr)
	case "search":
		var req contract.SearchRequest
		var kinds, files list
		fs.StringVar(&req.View, "view", "", "working|history|all")
		fs.StringVar(&req.TaskID, "task", "", "task filter")
		fs.StringVar(&req.SessionID, "session", "", "session filter")
		fs.Var(&kinds, "kind", "kind filter (repeatable)")
		fs.Var(&files, "file", "file relevance (repeatable)")
		fs.StringVar(&req.Since, "since", "", "RFC 3339 lower bound")
		fs.StringVar(&req.Until, "until", "", "RFC 3339 upper bound")
		fs.IntVar(&req.Limit, "limit", 0, "results per page")
		fs.StringVar(&req.Cursor, "cursor", "", "next_cursor from a previous page")
		pos, ok := parse()
		if !ok {
			return 2
		}
		req.Query, req.Kinds, req.Files, req.MaxBytes, req.Session = strings.Join(pos, " "), kinds, files, c.maxBytes, sessionHint()
		return call(false, "search", req)
	case "read":
		pos, ok := parse()
		if !ok {
			return 2
		}
		return call(false, "read", contract.ReadRequest{IDs: pos, MaxBytes: c.maxBytes})
	case "evidence":
		var lines int
		fs.IntVar(&lines, "lines", 0, "excerpt lines")
		pos, ok := parse()
		if !ok || len(pos) != 1 {
			return 2
		}
		return call(false, "evidence", contract.EvidenceRequest{ID: pos[0], ExcerptLines: lines, MaxBytes: c.maxBytes})
	case "capture":
		if _, ok := parse(); !ok {
			return 2
		}
		var req contract.CaptureRequest
		if err := contract.Decode(io.LimitReader(stdin, 1<<20), &req); err != nil {
			fmt.Fprintln(stdout, string(contract.EncodeError("capture", err.(*contract.Error))))
			return 2
		}
		req.Session = sessionHint()
		return call(false, "capture", req)
	case "update":
		var req contract.UpdateRequest
		fs.StringVar(&req.Status, "status", "", "disputed|invalidated")
		fs.StringVar(&req.Reason, "reason", "", "reason")
		pos, ok := parse()
		if !ok || len(pos) != 1 {
			return 2
		}
		req.ID = pos[0]
		return call(false, "update", req)
	case "task":
		var req contract.TaskRequest
		var stdinJSON bool
		fs.StringVar(&req.Title, "title", "", "task title (create)")
		fs.StringVar(&req.ProgressRef, "progress-ref", "", "project-relative progress file (create)")
		fs.Int64Var(&req.ExpectedRevision, "expected-revision", 0, "revision precondition")
		fs.StringVar(&req.To, "to", "", "target state (transition)")
		fs.StringVar(&req.Reason, "reason", "", "transition reason")
		fs.BoolVar(&stdinJSON, "checkpoint-stdin", false, "read checkpoint JSON from stdin")
		pos, ok := parse()
		if !ok || len(pos) == 0 {
			fmt.Fprintln(stderr, "usage: ichnora task list|create|checkpoint TASK|transition TASK")
			return 2
		}
		req.Action = pos[0]
		if len(pos) > 1 {
			req.TaskID = pos[1]
		}
		if stdinJSON {
			var cp contract.Checkpoint
			if err := contract.Decode(io.LimitReader(stdin, 1<<20), &cp); err != nil {
				fmt.Fprintln(stdout, string(contract.EncodeError("task", err.(*contract.Error))))
				return 2
			}
			req.Checkpoint = &cp
		}
		req.Session = sessionHint()
		return call(false, "task", req)
	case "resume":
		pos, ok := parse()
		if !ok || len(pos) != 1 {
			return 2
		}
		return call(false, "resume", contract.ResumeRequest{TaskID: pos[0], MaxBytes: c.maxBytes, Session: sessionHint()})
	case "status":
		if _, ok := parse(); !ok {
			return 2
		}
		return call(false, "status", contract.StatusRequest{MaxBytes: c.maxBytes})
	case "hook":
		pos, ok := parse()
		if !ok || len(pos) != 1 {
			return 2
		}
		cl, err := c.client(false)
		if err != nil {
			cl = server.Client{}
		}
		hook.Run(ctx, cl, pos[0], stdin, stdout)
		return 0
	case "mcp":
		var role string
		fs.StringVar(&role, "role", "", "narrow the agent socket's roles, e.g. reader")
		if _, ok := parse(); !ok {
			return 2
		}
		cl, err := c.client(false)
		if err != nil {
			fmt.Fprintln(stderr, "ichnora:", err)
			return 2
		}
		cl.Role = role
		srv := mcpbridge.Server(version, cl, sessionHint)
		if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "ichnora mcp:", err)
			return 1
		}
		return 0
	}
	return adminCommand(cmd, fs, parse, &c, call, stderr)
}

func adminCommand(cmd string, fs *flag.FlagSet, parse func() ([]string, bool), c *common,
	call func(bool, string, any) int, stderr io.Writer) int {
	var req contract.AdminRequest
	switch cmd {
	case "projects", "discover", "reconcile", "environment":
		if _, ok := parse(); !ok {
			return 2
		}
		return call(true, cmd, req)
	case "enroll":
		fs.StringVar(&req.Name, "name", "", "project name")
		fs.StringVar(&req.WorktreeOf, "worktree-of", "", "map PATH as a worktree of this project")
		fs.BoolVar(&req.Collect, "collect", false, "collect future sessions through harness hooks")
		fs.BoolVar(&req.Recall, "recall", false, "offer automatic recall at session boundaries")
		pos, ok := parse()
		if !ok || len(pos) != 1 {
			return 2
		}
		req.Path = pos[0]
	case "withdraw":
		pos, ok := parse()
		if !ok || len(pos) != 1 {
			return 2
		}
		req.Project = pos[0]
	case "policy":
		fs.StringVar(&req.Harness, "harness", "", "check as a transcript of this harness")
		pos, ok := parse()
		if !ok || len(pos) != 1 {
			return 2
		}
		req.Path = pos[0]
	case "share":
		fs.StringVar(&req.ToProject, "to", "", "target project")
		pos, ok := parse()
		if !ok || len(pos) != 1 {
			return 2
		}
		req.ClaimID = pos[0]
	case "revoke":
		pos, ok := parse()
		if !ok || len(pos) != 1 {
			return 2
		}
		req.ShareID = pos[0]
	case "backup":
		pos, ok := parse()
		if !ok || len(pos) != 1 {
			return 2
		}
		req.Out = pos[0]
	case "retention", "import":
		fs.StringVar(&req.Selector, "selector", "", "claim|session|project")
		fs.StringVar(&req.Target, "target", "", "selected ID")
		fs.StringVar(&req.Confirm, "confirm", "", "token from the preview")
		fs.StringVar(&req.Harness, "harness", "", "claude|codex|opencode")
		pos, ok := parse()
		if !ok || len(pos) == 0 || (pos[0] != "preview" && pos[0] != "apply") {
			fmt.Fprintf(stderr, "usage: ichnora %s preview|apply ...\n", cmd)
			return 2
		}
		req.Paths = pos[1:]
		return call(true, cmd+"_"+pos[0], req)
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	return call(true, cmd, req)
}
