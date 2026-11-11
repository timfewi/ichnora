package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timfewi/ichnora/internal/contract"
)

// A subprocess is required here: cancellation exercises graceful shutdown,
// whereas SIGKILL leaves SQLite's WAL and the state lock to crash recovery.
type serviceProcess struct {
	cmd  *exec.Cmd
	done chan error
	wait bool
}

func startServiceProcess(t *testing.T, e *testEnv) *serviceProcess {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "serve", "--config", e.cfgPath)
	cmd.Env = append(os.Environ(), "ICHNORA_TEST_MAIN=1")
	cmd.Stderr = e.logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &serviceProcess{cmd: cmd, done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if !p.wait {
			p.cmd.Process.Kill()
			<-p.done
			p.wait = true
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-p.done:
			p.wait = true
			t.Fatalf("service subprocess stopped before readiness: %v\n%s", err, e.logs.String())
		default:
		}
		conn, err := net.DialTimeout("unix", e.cfg.AgentSocket, 20*time.Millisecond)
		if err == nil {
			conn.Close()
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("service subprocess did not become ready: %s", e.logs.String())
	return nil
}

func (p *serviceProcess) kill(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-p.done; err == nil {
		t.Fatal("SIGKILL unexpectedly produced a clean exit")
	}
	p.wait = true
}

func TestSIGKILLPreservesAcknowledgedMemoryAndTranscriptCursor(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha, "--collect")
	e.stop()
	p := startServiceProcess(t, e)
	claim := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Acknowledged before SIGKILL"})[0]
	r := e.must(e.alpha, checkpointJSON(t, contract.Checkpoint{NextAction: "continue after SIGKILL"}),
		"task", "create", "--title", "Crash durable task", "--checkpoint-stdin")
	var task contract.TaskResult
	r.into(t, &task)
	session := "actual-crash"
	transcript := e.fixture("claude-transcript.jsonl", filepath.Join(e.claudeRoot, "crash.jsonl"), session, e.alpha)
	e.hook(e.alpha, "claude", map[string]any{"hook_event_name": "Stop", "session_id": session,
		"transcript_path": transcript, "cwd": e.alpha})
	events := count(e.must(e.alpha, "", "status").Raw, "events")
	if wal, err := os.Stat(filepath.Join(e.state, dbName) + "-wal"); err != nil || wal.Size() == 0 {
		t.Fatalf("test must crash with committed WAL records: %v", err)
	}
	p.kill(t)
	e.start()
	if got := e.must(e.alpha, "", "read", claim); !strings.Contains(got.Raw, claim) {
		t.Fatal("acknowledged claim lost after SIGKILL")
	}
	if got := e.must(e.alpha, "", "resume", task.Task.ID); !strings.Contains(got.Raw, "continue after SIGKILL") {
		t.Fatal("acknowledged checkpoint lost after SIGKILL")
	}
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"user","uuid":"post-crash","sessionId":%q,"cwd":%q,"message":{"content":"After actual SIGKILL"}}`, session, e.alpha)
	if _, err := fmt.Fprintln(f, line); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	e.must(e.root, "", "reconcile")
	if got := count(e.must(e.alpha, "", "status").Raw, "events"); got != events+1 {
		t.Fatalf("crash recovery lost or replayed cursor events: got %d, want %d", got, events+1)
	}
}
