package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type cancellationPhaseStep struct {
	pipeline.Step
	fixing bool
	seed   bool
}

func (s *cancellationPhaseStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if s.seed {
		return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"seed","severity":"warning","action":"ask-user","description":"retry validation"}]}`}, nil
	}
	sctx.Fixing = s.fixing
	if s.fixing {
		sctx.PreviousFindings = `{"findings":[{"id":"bug","severity":"error","action":"auto-fix","file":"feature.txt","description":"repair feature"}]}`
	}
	return s.Step.Execute(sctx)
}

func (s *cancellationPhaseStep) InterruptedWorkFindings(sctx *pipeline.StepContext) (string, error) {
	return s.Step.(pipeline.InterruptedWorkRecorder).InterruptedWorkFindings(sctx)
}

func TestExecutor_ActiveValidationCancellationRetainsWork(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		for _, cause := range []error{pipeline.ErrDaemonShutdown, fmt.Errorf(types.RunCancelReasonAbortedByUser)} {
			for _, phase := range []string{"review-fixer", "reviewer", "coverage", "test-repair", "test-evidence", "review-rebase", "test-rebase", "review-merge", "test-merge", "clean"} {
				t.Run(fmt.Sprintf("recovered=%t/%s/%s", recovered, cause, phase), func(t *testing.T) {
					dir, base, head := setupGitRepo(t)
					deadlineCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
					defer stop()
					ctx, cancel := context.WithCancelCause(deadlineCtx)
					defer cancel(nil)
					calls := 0
					ag := &mockAgent{name: "cancelled", runFn: func(agentCtx context.Context, opts agent.RunOpts) (*agent.Result, error) {
						calls++
						if phase == "coverage" && calls == 1 {
							return &agent.Result{Output: json.RawMessage(coverageFindingJSON([]string{}))}, nil
						}
						if phase == "clean" {
							cancel(cause)
							return nil, agentCtx.Err()
						}
						if strings.HasSuffix(phase, "rebase") {
							leaveConflictedRebase(t, dir)
						} else if strings.HasSuffix(phase, "merge") {
							gitCmd(t, dir, "checkout", "-b", "other")
							if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte("theirs"), 0o644); err != nil {
								t.Fatal(err)
							}
							gitCmd(t, dir, "add", "conflict.txt")
							gitCmd(t, dir, "commit", "-m", "theirs")
							gitCmd(t, dir, "checkout", "feature")
							if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte("ours"), 0o644); err != nil {
								t.Fatal(err)
							}
							gitCmd(t, dir, "add", "conflict.txt")
							gitCmd(t, dir, "commit", "-m", "ours")
							if _, err := runGitDirect(dir, "merge", "other"); err == nil {
								t.Fatal("merge should conflict")
							}
						} else {
							if err := os.WriteFile(filepath.Join(dir, "complete.txt"), []byte("complete commit"), 0o644); err != nil {
								t.Fatal(err)
							}
							gitCmd(t, dir, "add", "complete.txt")
							gitCmd(t, dir, "commit", "-m", "interrupted complete commit")
						}
						if err := os.WriteFile(filepath.Join(dir, "partial.txt"), []byte("partial work"), 0o644); err != nil {
							t.Fatal(err)
						}
						gitCmd(t, dir, "add", "partial.txt")
						if err := os.WriteFile(filepath.Join(dir, "partial.txt"), []byte("unstaged work"), 0o644); err != nil {
							t.Fatal(err)
						}
						cancel(cause)
						<-agentCtx.Done()
						return nil, agentCtx.Err()
					}}
					sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
					var actual pipeline.Step = &ReviewStep{}
					if strings.HasPrefix(phase, "test-") {
						actual = &TestStep{}
					}
					step := &cancellationPhaseStep{Step: actual, fixing: phase == "review-fixer" || phase == "test-repair"}
					appPaths := paths.WithRoot(t.TempDir())
					if recovered {
						seedCtx, seedCancel := context.WithCancelCause(deadlineCtx)
						seed := &cancellationPhaseStep{Step: actual, seed: true}
						seedExecutor := pipeline.NewExecutor(sctx.DB, appPaths, sctx.Config, ag, []pipeline.Step{seed}, func(event ipc.Event) {
							if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
								seedCancel(pipeline.ErrDaemonShutdown)
							}
						})
						if err := seedExecutor.Execute(seedCtx, sctx.Run, sctx.Repo, dir); !errors.Is(err, pipeline.ErrDaemonShutdown) {
							t.Fatalf("seed: %v", err)
						}
						seedCancel(nil)
						var err error
						sctx.Run, err = sctx.DB.GetRun(sctx.Run.ID)
						if err != nil {
							t.Fatal(err)
						}
					}
					var executor *pipeline.Executor
					executor = pipeline.NewExecutor(sctx.DB, appPaths, sctx.Config, ag, []pipeline.Step{step}, func(event ipc.Event) {
						if recovered && event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
							if err := executor.Respond(actual.Name(), types.ActionFix, nil); err != nil {
								t.Error(err)
							}
						}
					})
					var err error
					if recovered {
						err = executor.Resume(ctx, sctx.Run, sctx.Repo, dir)
					} else {
						err = executor.Execute(ctx, sctx.Run, sctx.Repo, dir)
					}
					if err == nil || calls == 0 {
						t.Fatalf("cancelled invocation returned err=%v calls=%d", err, calls)
					}
					if errors.Is(cause, pipeline.ErrDaemonShutdown) && !errors.Is(err, pipeline.ErrDaemonShutdown) {
						t.Fatalf("shutdown: %v", err)
					}
					results, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
					if phase == "clean" {
						if err != nil || len(results) != 1 {
							t.Fatalf("results=%+v err=%v", results, err)
						}
						if results[0].FindingsJSON != nil && pipeline.HasUnvalidatedWorkRefusal(*results[0].FindingsJSON) {
							t.Fatal("clean cancellation manufactured unvalidated work")
						}
						return
					}
					if err != nil || len(results) != 1 || results[0].FindingsJSON == nil || !pipeline.HasUnvalidatedWorkRefusal(*results[0].FindingsJSON) {
						t.Fatalf("retention results=%+v err=%v", results, err)
					}
					retained, err := types.ParseFindingsJSON(*results[0].FindingsJSON)
					if err != nil || retained.UnvalidatedSinceSHA != head {
						t.Fatalf("baseline=%+v err=%v", retained, err)
					}
					stored, err := sctx.DB.GetRun(sctx.Run.ID)
					committed := gitCmd(t, dir, "rev-parse", "HEAD")
					if strings.HasSuffix(phase, "rebase") || strings.HasSuffix(phase, "merge") {
						if err != nil || stored.HeadSHA != head {
							t.Fatalf("incomplete operation advanced custody=%+v err=%v", stored, err)
						}
						if strings.HasSuffix(phase, "rebase") && !rebaseInProgress(context.Background(), dir) {
							t.Fatal("interrupted rebase lost")
						}
						if strings.HasSuffix(phase, "merge") && !mergeInProgress(context.Background(), dir) {
							t.Fatal("interrupted merge lost")
						}
					} else {
						if err != nil || committed == head || stored.HeadSHA != committed {
							t.Fatalf("custody=%+v head=%s err=%v", stored, committed, err)
						}
						if actual.Name() == types.StepReview {
							uncertified, err := sctx.DB.GetUncertifiedPipelineRange(sctx.Repo.ID, sctx.Run.Branch)
							if err != nil || uncertified == nil || uncertified.FromSHA != head || uncertified.ToSHA != committed {
								t.Fatalf("uncertified=%+v err=%v", uncertified, err)
							}
						}
					}
					if got := gitCmd(t, dir, "show", ":partial.txt"); got != "partial work" {
						t.Fatalf("index=%q", got)
					}
					if got, err := os.ReadFile(filepath.Join(dir, "partial.txt")); err != nil || string(got) != "unstaged work" {
						t.Fatalf("working file=%q err=%v", got, err)
					}
				})
			}
		}
	}
}

func TestTestStep_NativeRepairTimeoutKeepsTimeoutClassification(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	var nativeErr error
	ag := &mockAgent{name: "native-repair", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAgentProcessExitHelper$")
		cmd.Env = append(os.Environ(), agentExitTestCodeEnv+"=hang")
		nativeErr = cmd.Run()
		return nil, nativeErr
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"bug","severity":"error","action":"auto-fix","description":"repair feature"}]}`
	sctx.Config.TestAgentTimeout = 200 * time.Millisecond
	outcome, err := (&TestStep{}).Execute(sctx)
	var exitErr *exec.ExitError
	if !errors.As(nativeErr, &exitErr) {
		t.Fatalf("native process result=%v", nativeErr)
	}
	if err != nil || outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	timeout := false
	for _, finding := range findings.Items {
		if finding.ID == types.FindingIDTestAgentTimeout {
			timeout = true
		}
		if finding.ID == types.FindingIDTestAgentIncomplete {
			t.Fatalf("timeout became process exit: %+v", findings)
		}
	}
	if !timeout {
		t.Fatalf("missing timeout: %+v", findings)
	}
	joined := errors.Join(errTestAgentTimeout, nativeErr)
	if isTestAgentProcessExit(joined) {
		t.Fatal("timeout classified as process exit")
	}
	classified := testAgentError(time.Second, "repair", joined)
	var invocation *testAgentInvocationError
	if !errors.As(classified, &invocation) || !invocation.timedOut {
		t.Fatalf("timeout diagnostic=%v", classified)
	}

	converted := testAgentProcessExitOutcome(sctx, joined, head, nil, "", 0)
	testFindingByID(t, converted.Findings, types.FindingIDTestAgentTimeout)
}
