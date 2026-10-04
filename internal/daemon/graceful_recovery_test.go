package daemon

import (
	"context"
	"fmt"
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

type shutdownRecoveryStep struct {
	stage  types.StepName
	active bool
}

func (s shutdownRecoveryStep) Name() types.StepName { return s.stage }
func (s shutdownRecoveryStep) Execute(sc *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := os.WriteFile(filepath.Join(sc.WorkDir, "unvalidated.txt"), []byte("preserve me"), 0600); err != nil {
		return nil, err
	}
	if s.active {
		if sc.Fixing {
			findings, err := types.ParseFindingsJSON(sc.PreviousFindings)
			if err != nil || (s.stage == types.StepTest && (findings.Verdict != "no-go" || findings.TestedHeadSHA != sc.Run.HeadSHA || len(findings.Scenarios) != 1 || findings.Scenarios[0].Result != "fail" || len(findings.Artifacts) != 1)) {
				return nil, fmt.Errorf("retry lost interrupted evidence: %+v, %v", findings, err)
			}
			return &pipeline.StepOutcome{ReviewedPaths: []string{"unvalidated.txt"}, ReviewablePaths: []string{"unvalidated.txt"}, ReviewApprovedHeadSHA: sc.Run.HeadSHA}, nil
		}
		payload, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{{ID: "pending", File: "unvalidated.txt", Severity: "error", Action: "auto-fix", Description: "unresolved failure"}}, Verdict: "no-go", TestedHeadSHA: sc.Run.HeadSHA, Scenarios: []types.TestScenario{{Name: "retained failure", Result: "fail", Live: true, Evidence: "retained.txt"}}, Artifacts: []types.TestArtifact{{Label: "failure receipt", Path: "retained.txt"}}})
		if err := sc.DB.SetStepFindings(sc.StepResultID, payload); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(sc.EvidenceDir, 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(sc.EvidenceDir, "retained.txt"), []byte("retained evidence"), 0600); err != nil {
			return nil, err
		}
		sc.Log("active validation evidence")
		if err := os.WriteFile(filepath.Join(sc.WorkDir, "active-ready"), nil, 0600); err != nil {
			return nil, err
		}
		<-sc.Ctx.Done()
		return &pipeline.StepOutcome{Findings: payload, ExitCode: 23}, context.Cause(sc.Ctx)
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
		stage  types.StepName
		fix    bool
		active bool
	}{{types.StepReview, false, false}, {types.StepReview, true, false}, {types.StepTest, false, false}, {types.StepCI, false, false}, {types.StepReview, false, true}, {types.StepTest, false, true}} {
		name := string(tc.stage)
		if tc.active {
			name += "-active"
		}
		if tc.fix {
			name += "-fix-review"
		}
		t.Run(name, func(t *testing.T) {
			sf := func() []pipeline.Step {
				return []pipeline.Step{shutdownRecoveryStep{stage: tc.stage, active: tc.active}}
			}
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
			if tc.stage == types.StepCI || tc.active {
				status = types.StepStatusRunning
			}
			initial := wait(status)
			if tc.active {
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(p.WorktreeDir(repo.ID, initial.ID), "active-ready")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("active turn did not start")
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
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
			if tc.active {
				status = types.StepStatusAwaitingApproval
			}
			results, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].Status != status {
				t.Fatalf("shutdown changed step: %+v", results)
			}
			if tc.active {
				if results[0].ExitCode == nil || *results[0].ExitCode != 23 || results[0].FindingsJSON == nil {
					t.Fatalf("lost interruption state: %+v", results[0])
				}
				findings, err := types.ParseFindingsJSON(*results[0].FindingsJSON)
				if err != nil || findings.Verdict != "no-go" || findings.TestedHeadSHA != head || len(findings.Scenarios) != 1 || findings.Scenarios[0].Result != "fail" || len(findings.Artifacts) != 1 || findings.Artifacts[0].Path != "retained.txt" {
					t.Fatalf("lost metadata: %+v %v", findings, err)
				}
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
			if tc.active {
				receipt := filepath.Join(p.RunEvidenceDir("", run.ID), "retained.txt")
				data, err := os.ReadFile(receipt)
				if err != nil || string(data) != "retained evidence" {
					t.Fatalf("restart lost evidence: %q %v", data, err)
				}
				log, err := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), string(tc.stage)+".log"))
				if err != nil || !strings.Contains(string(log), "active validation evidence") {
					t.Fatalf("restart lost invocation log: %q %v", log, err)
				}
				recoveredClient, err := ipc.Dial(p.Socket())
				if err != nil {
					t.Fatal(err)
				}
				defer recoveredClient.Close()
				var response ipc.RespondResult
				deadline := time.Now().Add(10 * time.Second)
				for {
					err = recoveredClient.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: run.ID, Step: tc.stage, Action: types.ActionFix, FindingIDs: []string{"pending", string(tc.stage) + "-agent-interrupted"}}, &response)
					if err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal(err)
					}
					time.Sleep(10 * time.Millisecond)
				}
				for {
					done, err := database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if done.Status == types.RunCompleted {
						return
					}
					if done.Status.Terminal() {
						t.Fatalf("retry failed: %+v", done)
					}
					if time.Now().After(deadline) {
						t.Fatal("retry did not complete")
					}
					time.Sleep(10 * time.Millisecond)
				}
			}

			data, err = os.ReadFile(filepath.Join(workDir, "unvalidated.txt"))
			if err != nil || string(data) != "preserve me" {
				t.Fatalf("restart discarded work: %q %v", data, err)
			}
		})
	}
}
