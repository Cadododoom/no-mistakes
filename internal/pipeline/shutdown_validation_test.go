package pipeline

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A clean interrupted turn still has not validated its head. Exercise the
// persisted shutdown gate itself, rather than seeding a recognized finding.
func TestExecutor_ShutdownValidationCannotApproveOrSkipCleanWork(t *testing.T) {
	for _, stage := range []types.StepName{types.StepReview, types.StepTest} {
		t.Run(string(stage), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			started := make(chan struct{})
			step := &adaptiveCallStep{name: stage, fn: func(sc *StepContext) (*StepOutcome, error) {
				close(started)
				<-sc.Ctx.Done()
				return nil, context.Cause(sc.Ctx)
			}}
			ctx, shutdown := context.WithCancelCause(context.Background())
			done := make(chan error, 1)
			workDir := t.TempDir()
			executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			go func() { done <- executor.Execute(ctx, run, repo, workDir) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				shutdown(ErrDaemonShutdown)
				t.Fatal("validation did not start")
			}
			shutdown(ErrDaemonShutdown)
			select {
			case err := <-done:
				if !errors.Is(err, ErrDaemonShutdown) {
					t.Fatalf("shutdown = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not park")
			}
			run, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			parked := make(chan struct{}, 1)
			id := types.FindingIDReviewAgentIncomplete
			if stage == types.StepTest {
				id = types.FindingIDTestAgentIncomplete
			}
			retry := &adaptiveCallStep{name: stage, fn: func(sc *StepContext) (*StepOutcome, error) {
				if !sc.Fixing || !hasFindingID(sc.PreviousFindings, id) {
					return nil, fmt.Errorf("retry lost invocation control finding: %s", sc.PreviousFindings)
				}
				return &StepOutcome{ReviewApprovedHeadSHA: sc.Run.HeadSHA}, nil
			}}
			executor = NewExecutor(database, p, nil, nil, []Step{retry}, func(event ipc.Event) {
				if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
					parked <- struct{}{}
				}
			})
			resumeCtx, cancel := context.WithCancel(context.Background())
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("recovered executor did not stop")
				}
			}()
			go func() { done <- executor.Resume(resumeCtx, run, repo, workDir) }()
			select {
			case <-parked:
			case <-time.After(5 * time.Second):
				t.Fatal("recovered validation did not park")
			}
			for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionSkip} {
				if err := executor.Respond(stage, action, nil); err == nil {
					t.Fatalf("%s bypassed interrupted validation", action)
				}
			}
			if err := executor.Respond(stage, types.ActionFix, []string{id}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				done <- err
				if err != nil {
					t.Fatalf("validation retry = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("invocation control finding stranded successful retry")
			}
		})
	}
}
