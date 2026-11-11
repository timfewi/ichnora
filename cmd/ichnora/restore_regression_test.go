package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timfewi/ichnora/internal/config"
	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/store"
)

func TestRestorePreservesCommittedWALInPreviousSnapshot(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	before := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Before backup"})[0]
	backup := filepath.Join(e.root, "backup.db")
	e.must(e.root, "", "backup", backup)
	e.stop()
	p := startServiceProcess(t, e)
	after := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Committed only in old WAL"})[0]
	p.kill(t)
	var out, stderr bytes.Buffer
	if code := restore(e.cfgPath, backup, &out, &stderr); code != 0 {
		t.Fatalf("restore after SIGKILL: %s", stderr.String())
	}
	previous, err := filepath.Glob(filepath.Join(e.state, dbName+".previous-*"))
	if err != nil || len(previous) != 1 {
		t.Fatalf("previous snapshots: %v %v", previous, err)
	}
	if _, err := store.Verify(context.Background(), previous[0]); err != nil {
		t.Fatalf("previous snapshot is not self-contained: %v", err)
	}
	st, err := store.Open(context.Background(), previous[0], store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	err = st.Read().QueryRow(`SELECT count(*) FROM claims WHERE id = ?`, after).Scan(&n)
	st.Close()
	if err != nil || n != 1 {
		t.Fatalf("previous snapshot lost the acknowledged WAL claim: count=%d err=%v", n, err)
	}
	e.start()
	r := e.must(e.alpha, "", "read", before, after)
	var records contract.ReadResult
	r.into(t, &records)
	if len(records.Records) != 1 || records.Records[0].ID != before || len(records.Missing) != 1 || records.Missing[0] != after {
		t.Fatalf("replacement mixed old WAL with restored state: %s", r.Raw)
	}
}

func TestRestoreMigrationFailureLeavesCanonicalDatabase(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	claim := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Keep original on migration failure"})[0]
	backup := filepath.Join(e.root, "incompatible.db")
	e.must(e.root, "", "backup", backup)
	e.stop()
	// The file passes integrity/version checks, but replaying migration 1
	// against its existing tables must fail before the live database changes.
	db, err := sql.Open("ichnora_sqlite3", "file:"+backup+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := restore(e.cfgPath, backup, &bytes.Buffer{}, &stderr); code == 0 {
		t.Fatal("incompatible staged migration unexpectedly succeeded")
	}
	e.start()
	if got := e.must(e.alpha, "", "read", claim); !strings.Contains(got.Raw, claim) {
		t.Fatal("failed staged migration changed the live database")
	}
}

func TestRestoreReplacesCorruptDatabaseAndRetainsOriginalBytes(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	claim := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Recover from a valid backup"})[0]
	backup := filepath.Join(e.root, "valid.db")
	e.must(e.root, "", "backup", backup)
	e.stop()
	corrupt := []byte("synthetic corrupt database, not a SQLite header")
	target := filepath.Join(e.state, dbName)
	if err := os.WriteFile(target, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	wal := []byte("synthetic retained WAL bytes")
	if err := os.WriteFile(target+"-wal", wal, 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := restore(e.cfgPath, backup, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("valid backup could not replace corrupt current database: %s", stderr.String())
	}
	previous, err := filepath.Glob(target + ".previous-*")
	if err != nil || len(previous) != 1 {
		t.Fatalf("corrupt previous-state copies: %v %v", previous, err)
	}
	for suffix, want := range map[string][]byte{"": corrupt, "-wal": wal} {
		got, err := os.ReadFile(filepath.Join(previous[0], dbName+suffix))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("corrupt state %q was not preserved byte-for-byte: %v", suffix, err)
		}
	}
	e.start()
	if got := e.must(e.alpha, "", "read", claim); !strings.Contains(got.Raw, claim) {
		t.Fatal("valid backup was not installed after corrupt-state preservation")
	}
}

func TestRestoreInterruptedPreparationCanRestartAndRetry(t *testing.T) {
	if path := os.Getenv("ICHNORA_TEST_RESTORE_PREPARE_CONFIG"); path != "" {
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		lock, err := prepareState(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if _, err := stageRestore(context.Background(), cfg.StateDir, os.Getenv("ICHNORA_TEST_RESTORE_BACKUP")); err != nil {
			t.Fatal(err)
		}
		if _, err := preserveDatabase(context.Background(), filepath.Join(cfg.StateDir, dbName)); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "prepared")
		for {
			time.Sleep(time.Hour)
		}
	}
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	before := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Selected restore state"})[0]
	backup := filepath.Join(e.root, "selected.db")
	e.must(e.root, "", "backup", backup)
	after := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Live state during interrupted restore"})[0]
	e.stop()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestRestoreInterruptedPreparationCanRestartAndRetry$")
	cmd.Env = append(os.Environ(), "ICHNORA_TEST_RESTORE_PREPARE_CONFIG="+e.cfgPath, "ICHNORA_TEST_RESTORE_BACKUP="+backup)
	cmd.Stderr = e.logs
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	select {
	case line := <-ready:
		if line != "prepared" {
			t.Fatalf("restore helper failed before preparation: %q\n%s", line, e.logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("restore helper did not prepare")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("restore helper exited cleanly instead of SIGKILL")
	}
	waited = true
	if _, err := os.Stat(filepath.Join(e.state, dbName)); err != nil {
		t.Fatalf("interrupted restore removed canonical database: %v", err)
	}
	e.start()
	var records contract.ReadResult
	e.must(e.alpha, "", "read", before, after).into(t, &records)
	if len(records.Records) != 2 {
		t.Fatal("restart after interrupted preparation lost the original state")
	}
	e.stop()
	var stderr bytes.Buffer
	if code := restore(e.cfgPath, backup, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("retry blocked by interrupted staging: %s", stderr.String())
	}
	e.start()
	e.must(e.alpha, "", "read", before, after).into(t, &records)
	if len(records.Records) != 1 || records.Records[0].ID != before {
		t.Fatalf("retry did not install selected backup: %+v", records)
	}
}

func TestServiceRefusesEmptyDatabaseWhenRestoreArtifactsRemain(t *testing.T) {
	for _, artifact := range []string{dbName + ".restore", dbName + ".previous-recoverable", ".ichnora-restore-interrupted", dbName + "-wal"} {
		t.Run(artifact, func(t *testing.T) {
			e := newEnv(t, nil)
			e.stop()
			if err := os.Remove(filepath.Join(e.state, dbName)); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(e.state, artifact), "synthetic recovery artifact")
			if err := checkRestoreState(e.state); err == nil {
				t.Fatal("missing database and restore artifact accepted as fresh state")
			}
			if _, err := os.Stat(filepath.Join(e.state, dbName)); !os.IsNotExist(err) {
				t.Fatal("checking recovery state created an empty database")
			}
		})
	}
}
