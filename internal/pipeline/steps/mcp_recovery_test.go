package steps

import (
 "context"
 "encoding/json"
 "os"
 "path/filepath"
 "testing"

 "github.com/kunchenguid/no-mistakes/internal/agent"
 "github.com/kunchenguid/no-mistakes/internal/config"
 "github.com/kunchenguid/no-mistakes/internal/pipeline"
 "github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewStep_CoverageAuthorizationFailureHandsOff(t *testing.T) {
 dir, base, head := setupGitRepo(t)
 calls := 0
 ag := &mcpTestAgent{mockAgent: &mockAgent{name: "mcp", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
  calls++
  if calls == 1 {
   return &agent.Result{Output: json.RawMessage(coverageFindingJSON([]string{}))}, nil
  }
  if opts.Purpose != "review-coverage" { t.Fatalf("purpose = %q", opts.Purpose) }
  return nil, &agent.MCPAuthorizationError{Server: "cloudflare"}
 }}}
 sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
 sctx.StepName = types.StepReview
 sctx.Run.RequiredMCPJSON = `[{"stage":"review","server":"cloudflare"}]`
 sctx.MCPProber = &fakeMCPReadinessProber{result: types.MCPProbeResult{Status: types.MCPStatusAuthorized}}
 outcome, err := (&ReviewStep{}).Execute(sctx)
 if err != nil || outcome == nil || !outcome.NeedsApproval || !pipeline.HasMCPAuthorizationRefusal(outcome.Findings) || calls != 2 {
  t.Fatalf("outcome=%+v err=%v calls=%d", outcome, err, calls)
 }
 finding := testFindingByID(t, outcome.Findings, types.FindingIDMCPAuthorizationRequired)
 if finding.AuthorizationRequired == nil || finding.AuthorizationRequired.Status != types.MCPStatusAuthorizationRequiredDuringTool { t.Fatalf("handoff = %+v", finding) }
}

func TestReviewStep_InterruptedCommitRecordsRecoverableHead(t *testing.T) {
 for _, phase := range []string{"fixer", "reviewer", "coverage"} {
  t.Run(phase, func(t *testing.T) {
   dir, base, head := setupGitRepo(t)
   calls := 0
   ag := &mockAgent{name: "interrupted", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
    calls++
    if phase == "coverage" && calls == 1 { return &agent.Result{Output: json.RawMessage(coverageFindingJSON([]string{}))}, nil }
    if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("retained commit"), 0o644); err != nil { t.Fatal(err) }
    gitCmd(t, dir, "add", "committed.txt")
    gitCmd(t, dir, "commit", "-m", "interrupted agent commit")
    return nil, errReviewAgentTimeout
   }}
   sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
   if phase == "fixer" {
    sctx.Fixing = true
    sctx.PreviousFindings = `{"findings":[{"id":"bug","file":"feature.txt","severity":"error","action":"auto-fix","description":"fix it"}]}`
   }
   outcome, err := (&ReviewStep{}).Execute(sctx)
   if err != nil || outcome == nil || !outcome.NeedsApproval || !pipeline.HasUnvalidatedWorkRefusal(outcome.Findings) { t.Fatalf("outcome=%+v err=%v", outcome, err) }
   committed := gitCmd(t, dir, "rev-parse", "HEAD")
   stored, err := sctx.DB.GetRun(sctx.Run.ID)
   if err != nil || committed == head || stored.HeadSHA != committed || sctx.Run.HeadSHA != committed { t.Fatalf("head=%s stored=%+v err=%v", committed, stored, err) }
   recorded, err := sctx.DB.GetUncertifiedPipelineRange(sctx.Repo.ID, sctx.Run.Branch)
   if err != nil || recorded == nil || recorded.FromSHA != head || recorded.ToSHA != committed { t.Fatalf("uncertified range=%+v err=%v", recorded, err) }
   if gitCmd(t, dir, "rev-parse", sctx.Run.Branch) != committed { t.Fatal("branch custody did not retain committed head") }
  })
 }
}

func TestMCPAuthorization_RetainsInterruptedWorkAcrossRetries(t *testing.T) {
 for _, stage := range []types.StepName{types.StepReview, types.StepTest} {
  for _, phase := range []string{"evidence", "fixer"} {
   t.Run(string(stage)+"/"+phase, func(t *testing.T) {
    dir, base, head := setupGitRepo(t)
    calls := 0
    partial := filepath.Join(dir, "partial.txt")
    ag := &mcpTestAgent{mockAgent: &mockAgent{name: "mcp", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
     calls++
     if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("retained commit"), 0o644); err != nil { t.Fatal(err) }
     gitCmd(t, dir, "add", "committed.txt")
     gitCmd(t, dir, "commit", "-m", "auth-interrupted commit")
     if err := os.WriteFile(partial, []byte("partial work"), 0o644); err != nil { t.Fatal(err) }
     return nil, &agent.MCPAuthorizationError{Server: "cloudflare"}
    }}}
    sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Test: "exit 0"})
    sctx.StepName = stage
    sctx.Run.RequiredMCPJSON, _ = types.MarshalMCPRequirements([]types.MCPRequirement{{Stage: stage, Server: "cloudflare"}})
    prober := &fakeMCPReadinessProber{result: types.MCPProbeResult{Status: types.MCPStatusAuthorized}}
    sctx.MCPProber = prober
    var step pipeline.Step = &ReviewStep{}
    if stage == types.StepTest { step = &TestStep{} }
    if phase == "fixer" {
     sctx.Fixing = true
     sctx.PreviousFindings = `{"findings":[{"id":"bug","file":"feature.txt","severity":"error","action":"auto-fix","description":"fix it"}]}`
    }
    first, err := step.Execute(sctx)
    if err != nil || first == nil || !pipeline.HasMCPAuthorizationRefusal(first.Findings) || !pipeline.HasUnvalidatedWorkRefusal(first.Findings) { t.Fatalf("first=%+v err=%v", first, err) }
    committed := gitCmd(t, dir, "rev-parse", "HEAD")
    stored, err := sctx.DB.GetRun(sctx.Run.ID)
    if err != nil || stored.HeadSHA != committed || committed == head { t.Fatalf("custody=%+v err=%v", stored, err) }
    sctx.Fixing = true
    sctx.PreviousFindings = first.Findings
    prober.result.Status = types.MCPStatusAuthorizationRequiredDuringProbe
    second, err := step.Execute(sctx)
    if err != nil || second == nil || !pipeline.HasMCPAuthorizationRefusal(second.Findings) || !pipeline.HasUnvalidatedWorkRefusal(second.Findings) || calls != 1 { t.Fatalf("second=%+v err=%v calls=%d", second, err, calls) }
    if content, err := os.ReadFile(partial); err != nil || string(content) != "partial work" { t.Fatalf("partial work=%q err=%v", content, err) }
    findings, err := types.ParseFindingsJSON(second.Findings)
    if err != nil || findings.UnvalidatedSinceSHA != head { t.Fatalf("baseline=%s err=%v", findings.UnvalidatedSinceSHA, err) }
   })
  }
 }
}

func TestInterruptedWork_NeverRecordsUnfinishedGitOperation(t *testing.T) {
 for _, stage := range []types.StepName{types.StepReview, types.StepTest} {
  for _, operation := range []string{"rebase", "merge"} {
   t.Run(string(stage)+"/"+operation, func(t *testing.T) {
    dir, base, head := setupGitRepo(t)
    sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, base, head, config.Commands{})
    if operation == "rebase" {
     leaveConflictedRebase(t, dir)
    } else {
     gitCmd(t, dir, "checkout", "-b", "other")
     if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte("theirs"), 0o644); err != nil { t.Fatal(err) }
     gitCmd(t, dir, "add", "conflict.txt")
     gitCmd(t, dir, "commit", "-m", "theirs")
     gitCmd(t, dir, "checkout", "feature")
     if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte("ours"), 0o644); err != nil { t.Fatal(err) }
     gitCmd(t, dir, "add", "conflict.txt")
     gitCmd(t, dir, "commit", "-m", "ours")
     if _, err := runGitDirect(dir, "merge", "other"); err == nil { t.Fatal("merge should conflict") }
    }
    outcome := mcpAuthorizationOutcome(sctx, stage, types.MCPProbeResult{NextAction: "login"}, false)
    stored, err := sctx.DB.GetRun(sctx.Run.ID)
    if err != nil || stored.HeadSHA != head || sctx.Run.HeadSHA != head || !pipeline.HasUnvalidatedWorkRefusal(outcome.Findings) { t.Fatalf("head=%+v outcome=%+v err=%v", stored, outcome, err) }
    if operation == "rebase" && !rebaseInProgress(sctx.Ctx, dir) { t.Fatal("interrupted rebase changed") }
    if operation == "merge" && !mergeInProgress(sctx.Ctx, dir) { t.Fatal("interrupted merge changed") }
   })
  }
 }
}

func TestMCPPreflight_KeepsExistingRetentionWithoutBaselineMetadata(t *testing.T) {
 dir, base, head := setupGitRepo(t)
 sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, base, head, config.Commands{})
 sctx.PreviousFindings = `{"findings":[{"id":"test-agent-unvalidated-work","severity":"error","action":"ask-user","description":"committed work from a previous turn"}]}`
 previous, err := types.ParseFindingsJSON(sctx.PreviousFindings)
 if err != nil { t.Fatal(err) }
 previous.Items[0].ID = types.FindingIDTestAgentUnvalidatedWork
 sctx.PreviousFindings, _ = types.MarshalFindingsJSON(previous)
 outcome := mcpAuthorizationOutcome(sctx, types.StepTest, types.MCPProbeResult{NextAction: "login"}, false)
 if !pipeline.HasUnvalidatedWorkRefusal(outcome.Findings) { t.Fatal("retention marker dropped") }

}
