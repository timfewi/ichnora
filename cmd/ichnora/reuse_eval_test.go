package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timfewi/ichnora/internal/contract"
)

// The default editor exercises the scoring protocol. An explicitly selected
// external runner can solve the same tasks with any of the three contexts.
func TestPairedCodingReuse(t *testing.T) {
	const budget = 2048
	const task = "Add a probe argument to the active handler. It must print ready. Preserve normal dispatch, the dispatcher, route selection and the standby handler."
	runner := os.Getenv("ICHNORA_REUSE_RUNNER")
	if runner != "" {
		if !filepath.IsAbs(runner) {
			t.Fatal("ICHNORA_REUSE_RUNNER must be an absolute executable path")
		}
		if _, err := exec.LookPath(runner); err != nil {
			t.Fatal(err)
		}
	}
	for i, scenario := range []string{"stable", "switched", "no-evidence"} {
		// Rotate condition order; start each paired task from identical files.
		conditions := []string{"none", "notes", "ichnora"}
		for j := range conditions {
			condition := conditions[(i+j)%len(conditions)]
			t.Run(scenario+"/"+condition, func(t *testing.T) {
				e := newEnv(t, nil)
				e.enroll(e.alpha)
				const before = "printf '%s\\n' normal\n"
				const dispatcher = ". ./route.conf\nexec sh \"./handlers/$handler\" \"$@\"\n"
				for _, name := range []string{"left", "right"} {
					writeFile(t, filepath.Join(e.alpha, "handlers", name), before)
				}
				route := filepath.Join(e.alpha, "route.conf")
				active, standby := "left", "right"
				writeFile(t, route, "handler="+active+"\n")
				writeFile(t, filepath.Join(e.alpha, "dispatch.sh"), dispatcher)
				var note string
				if scenario != "no-evidence" {
					note = "The active handler is handlers/left."
					ref := []contract.Ref{{Kind: "file", Path: "route.conf"}}
					e.captureIDs(e.alpha, contract.ClaimInput{Kind: "observation", Title: note, Depends: ref, Support: ref})
				}
				if scenario == "switched" {
					active, standby = standby, active
					writeFile(t, route, "handler="+active+"\n")
				}
				start := time.Now()
				var memory string
				switch condition {
				case "notes":
					memory = note
				case "ichnora":
					r := e.must(e.alpha, "", "search", "active handler", "--max-bytes", "2048")
					memory = r.Raw
					var search contract.SearchResult
					r.into(t, &search)
					if (search.Totals.Working == 1) != (scenario == "stable") {
						t.Fatalf("incorrect working context: %+v", search)
					}
				}
				if len(memory) > budget {
					t.Fatal("context exceeds the common byte budget")
				}
				input, _ := json.Marshal(map[string]string{"task": task, "memory": memory})
				runnerOK := true
				if runner != "" {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
					defer cancel()
					cmd := exec.CommandContext(ctx, runner)
					cmd.Dir = e.alpha
					cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "LANG=C", "LC_ALL=C"}
					cmd.Stdin = bytes.NewReader(input)
					if err := cmd.Run(); err != nil {
						var exit *exec.ExitError
						if !errors.As(err, &exit) {
							t.Fatal(err)
						}
						runnerOK = false
					}
				} else {
					data, err := os.ReadFile(route)
					if err != nil {
						t.Fatal(err)
					}
					name := strings.TrimPrefix(strings.TrimSpace(string(data)), "handler=")
					writeFile(t, filepath.Join(e.alpha, "handlers", name), "if [ \"$1\" = probe ]; then echo ready; exit; fi\n"+before)
				}
				elapsed := time.Since(start)
				unchanged := func(path, want string) bool {
					b, err := os.ReadFile(filepath.Join(e.alpha, path))
					return err == nil && string(b) == want
				}
				dispatch := func(arg, want string) bool {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, "sh", "dispatch.sh", arg)
					cmd.Dir = e.alpha
					b, err := cmd.Output()
					return err == nil && string(b) == want+"\n"
				}
				standbyEdited := !unchanged("handlers/"+standby, before)
				solved := runnerOK && !standbyEdited && unchanged("dispatch.sh", dispatcher) && unchanged("route.conf", "handler="+active+"\n") && dispatch("probe", "ready") && dispatch("normal", "normal")
				actor := "reference"
				if runner != "" {
					actor = "external"
				}
				t.Logf("actor=%s runner_ok=%t solved=%t standby_edited=%t context_bytes=%d elapsed_ms=%d", actor, runnerOK, solved, standbyEdited, len(memory), elapsed.Milliseconds())
				if runner == "" && !solved {
					t.Fatalf("reference edit failed the %s scoring fixture", scenario)
				}
			})
		}
	}
}
