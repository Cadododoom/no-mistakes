package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type shutdownRecoveryStep struct{ stage types.StepName }

func (s shutdownRecoveryStep) Name() types.StepName { return s.stage }
func (s shutdownRecoveryStep) Execute(sc *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := os.WriteFile(filepath.Join(sc.WorkDir, "unvalidated.txt"), []byte("preserve me"), 0600); err != nil {
		return nil, err
	}
	if s.stage == types.StepCI {
		if err := sc.DB.UpdateRunPRURL(sc.Run.ID, "https://github.com/test/shutdown-recovery/pull/42"); err != nil {
			return nil, err
		}
		<-sc.Ctx.Done()
		return nil, context.Cause(sc.Ctx)
	}
	payload, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{{ID: "pending", File: "unvalidated.txt", Severity: "warning", Action: "ask-user", Description: "operator decision pending"}}})
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: payload}, nil
}

func TestGracefulShutdownPreservesDurableGatesAndWork(t *testing.T) {
	for _, tc := range []struct {
		stage types.StepName
		fix   bool
	}{{types.StepReview, false}, {types.StepReview, true}, {types.StepTest, false}, {types.StepCI, false}} {
		name := string(tc.stage)
		if tc.fix {
			name += "-fix-review"
		}
		t.Run(name, func(t *testing.T) {
			sf := func() []pipeline.Step { return []pipeline.Step{shutdownRecoveryStep{stage: tc.stage}} }
			p, database, stopped := startTestDaemonWithStepsAndStopSignal(t, sf)
			repo, head := setupTestGitRepo(t, p, database, "shutdown-recovery")
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var pushed ipc.PushReceivedResult
			if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", Old: strings.Repeat("0", 40), New: head}, &pushed); err != nil {
				t.Fatal(err)
			}
			wait := func(status types.StepStatus) *db.Run {
				t.Helper()
				deadline := time.Now().Add(30 * time.Second)
				for time.Now().Before(deadline) {
					run, err := database.GetRun(pushed.RunID)
					if err != nil {
						t.Fatal(err)
					}
					results, err := database.GetStepsByRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if len(results) == 1 && results[0].Status == status {
						return run
					}
					if run.Status.Terminal() {
						t.Fatalf("unexpected terminal run: %+v", run)
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatal("stage did not reach expected status")
				return nil
			}
			status := types.StepStatusAwaitingApproval
			if tc.stage == types.StepCI {
				status = types.StepStatusRunning
			}
			wait(status)
			if tc.fix {
				var response ipc.RespondResult
				if err := client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: pushed.RunID, Step: types.StepReview, Action: types.ActionFix, FindingIDs: []string{"pending"}}, &response); err != nil {
					t.Fatal(err)
				}
				status = types.StepStatusFixReview
				wait(status)
			}
			if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil); err != nil {
				t.Fatal(err)
			}
			client.Close()
			// Socket removal precedes the deferred singleton-lock release.
			// Wait for the actual shutdown before starting its replacement.
			select {
			case <-stopped:
			case <-time.After(30 * time.Second):
				t.Fatal("isolated daemon did not finish shutdown")
			}
			deadline := time.Now().Add(30 * time.Second)
			for {
				if _, err := os.Stat(p.Socket()); os.IsNotExist(err) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("isolated daemon did not stop")
				}
				time.Sleep(10 * time.Millisecond)
			}
			run, err := database.GetRun(pushed.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != types.RunRunning || run.Error != nil {
				t.Fatalf("shutdown terminalized durable run: %+v", run)
			}
			if tc.stage != types.StepCI && run.AwaitingAgentSince == nil {
				t.Fatal("shutdown cleared parked gate timestamp")
			}
			results, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].Status != status {
				t.Fatalf("shutdown changed step: %+v", results)
			}
			workDir := p.WorktreeDir(repo.ID, run.ID)
			data, err := os.ReadFile(filepath.Join(workDir, "unvalidated.txt"))
			if err != nil || string(data) != "preserve me" {
				t.Fatalf("shutdown discarded work: %q %v", data, err)
			}
			// Start a second isolated daemon: parked gates resume through real trusted
			// recovery; a CI monitor retains its explicit interrupted outcome.
			runTestDaemon(t, p, database, sf, 30*time.Second)
			if tc.stage == types.StepCI {
				recovered, err := database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if recovered.Status != types.RunCIMonitorInterrupted {
					t.Fatalf("CI restart status=%s", recovered.Status)
				}
			} else {
				wait(status)
			}
			data, err = os.ReadFile(filepath.Join(workDir, "unvalidated.txt"))
			if err != nil || string(data) != "preserve me" {
				t.Fatalf("restart discarded work: %q %v", data, err)
			}
		})
	}
}
