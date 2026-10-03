package pipeline

import (
 "context"
 "errors"
 "os"
 "path/filepath"
 "strings"
 "testing"
 "time"

 "github.com/kunchenguid/no-mistakes/internal/ipc"
 "github.com/kunchenguid/no-mistakes/internal/types"
)

type launchMCPProber struct {
 fn func(context.Context, string, string) (types.MCPProbeResult, error)
}

func (p launchMCPProber) ProbeMCP(ctx context.Context, cwd, server string) (types.MCPProbeResult, error) {
 return p.fn(ctx, cwd, server)
}

func TestExecutor_PrelaunchMCPReadiness(t *testing.T) {
 for _, mode := range []string{"live", "recovered", "skipped-first-step"} {
  t.Run(mode, func(t *testing.T) {
   database, p, _, repo := setupTest(t)
   run, err := database.InsertRunWithIntentAndLaunchNonceAndMCP(repo.ID, "mcp", "abc123", "def456", nil, "", "", "", "", false, nil, []types.MCPRequirement{
    {Stage: types.StepReview, Server: "cloudflare"},
    {Stage: types.StepTest, Server: "cloudflare"},
   })
   if err != nil { t.Fatal(err) }
   cwd := t.TempDir()
   calls, probes, gates := 0, 0, 0
   ready := false
   prober := launchMCPProber{fn: func(_ context.Context, workDir, server string) (types.MCPProbeResult, error) {
    probes++
    if workDir != cwd || server != "cloudflare" { t.Fatalf("probe context = %q %q", workDir, server) }
    status := types.MCPStatusAuthorized
    if probes%2 == 0 && !ready { status = types.MCPStatusAuthorizationRequiredDuringProbe }
    return types.MCPProbeResult{Status: status, ExecutorContext: "daemon test context", NextAction: "safe login instruction"}, nil
   }}
   pass := func(name types.StepName) Step {
    return &adaptiveCallStep{name: name, fn: func(sctx *StepContext) (*StepOutcome, error) {
     calls++
     if !ready || sctx.Fixing || sctx.PreviousFindings != "" { t.Fatalf("work launched with readiness=%v fixing=%v findings=%q", ready, sctx.Fixing, sctx.PreviousFindings) }
     return &StepOutcome{}, nil
    }}
   }
   steps := []Step{pass(types.StepRebase), pass(types.StepReview), pass(types.StepTest)}
   gateStep := types.StepRebase
   if mode == "skipped-first-step" { gateStep = types.StepReview }
   ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
   defer cancel()
   launchCtx, shutdown := context.WithCancelCause(ctx)
   defer shutdown(nil)
   var executor *Executor
   handler := func(event ipc.Event) {
    if event.Type != ipc.EventStepCompleted || event.Status == nil || *event.Status != string(types.StepStatusAwaitingApproval) { return }
    gates++
    if calls != 0 || event.StepName == nil || *event.StepName != gateStep { t.Fatalf("gate after work or on wrong step: calls=%d event=%+v", calls, event) }
    results, err := database.GetStepsByRun(run.ID)
    if err != nil { t.Fatal(err) }
    var raw string
    for _, result := range results { if result.StepName == gateStep && result.FindingsJSON != nil { raw = *result.FindingsJSON } }
    findings, err := types.ParseFindingsJSON(raw)
    if err != nil || len(findings.Items) != 1 { t.Fatalf("handoff = %q, %v", raw, err) }
    auth := findings.Items[0].AuthorizationRequired
    if auth == nil || auth.Stage != "test" || auth.ExecutorContext != "daemon test context" || auth.NextAction != "safe login instruction" { t.Fatalf("handoff = %+v", auth) }
    for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionSkip} {
     if err := executor.Respond(gateStep, action, nil); err == nil { t.Fatalf("%s bypassed readiness", action) }
    }
    if mode == "recovered" && gates == 1 { shutdown(ErrDaemonShutdown); return }
    if gates == 2 { ready = true }
    if err := executor.Respond(gateStep, types.ActionFix, nil); err != nil { t.Fatal(err) }
   }
   executor = NewExecutor(database, p, nil, nil, steps, handler)
   executor.SetMCPReadinessProber(prober)
   if mode == "skipped-first-step" { executor.SetSkippedSteps([]types.StepName{types.StepRebase}) }
   err = executor.Execute(launchCtx, run, repo, cwd)
   if mode == "recovered" {
    if !errors.Is(err, ErrDaemonShutdown) { t.Fatalf("shutdown = %v", err) }
    run, err = database.GetRun(run.ID)
    if err != nil { t.Fatal(err) }
    executor = NewExecutor(database, p, nil, nil, steps, handler)
    executor.SetMCPReadinessProber(prober)
    err = executor.Resume(ctx, run, repo, cwd)
   }
   if err != nil { t.Fatal(err) }
   expectedCalls := 3
   expectedProbes := 6
   // Resume re-emits the durable gate before retrying; it does not probe
   // while the operator's prerequisite decision is still outstanding.
   if mode == "recovered" { expectedProbes = 4 }
   if mode == "skipped-first-step" { expectedCalls = 2 }
   if calls != expectedCalls || gates != 2 || probes != expectedProbes { t.Fatalf("calls=%d gates=%d probes=%d", calls, gates, probes) }
  })
 }
}

func TestExecutor_PrelaunchMCPProbeErrorIsPrivate(t *testing.T) {
 database, p, run, repo := setupTest(t)
 run.RequiredMCPJSON = `[{"stage":"test","server":"cloudflare"}]`
 step := newPassStep(types.StepRebase)
 var executor *Executor
 executor = NewExecutor(database, p, nil, nil, []Step{step}, func(event ipc.Event) {
  if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
   if err := executor.Respond(types.StepRebase, types.ActionAbort, nil); err != nil { t.Error(err) }
  }
 })
 executor.SetMCPReadinessProber(launchMCPProber{fn: func(context.Context, string, string) (types.MCPProbeResult, error) {
  return types.MCPProbeResult{}, errors.New("secret-token-from-provider")
 }})
 if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err == nil { t.Fatal("expected aborted prerequisite gate") }
 if step.callCount() != 0 { t.Fatal("pipeline work ran with failed probe") }
 log, err := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), "rebase.log"))
 if err != nil { t.Fatal(err) }
 results, err := database.GetStepsByRun(run.ID)
 if err != nil { t.Fatal(err) }
 if strings.Contains(string(log), "secret-token") || (results[0].FindingsJSON != nil && strings.Contains(*results[0].FindingsJSON, "secret-token")) { t.Fatal("raw provider error was published") }
}

func TestExecutor_PrelaunchUndeclaredMCPDoesNotProbe(t *testing.T) {
 database, p, run, repo := setupTest(t)
 step := newPassStep(types.StepRebase)
 executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
 executor.SetMCPReadinessProber(launchMCPProber{fn: func(context.Context, string, string) (types.MCPProbeResult, error) {
  t.Fatal("optional MCP was probed")
  return types.MCPProbeResult{}, nil
 }})
 if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil { t.Fatal(err) }
 if step.callCount() != 1 { t.Fatal("ordinary pipeline did not execute") }
}
