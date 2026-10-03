package steps

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type fakeMCPReadinessProber struct {
	result types.MCPProbeResult
	err    error
	calls  int
	server string
	cwd    string
}

func TestReviewMCPAuthorizationRetryReprobesBeforeLaunchingAgent(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	agentCalls := 0
	ag := &mcpTestAgent{mockAgent: &mockAgent{name: "mock", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		agentCalls++
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"reviewed_paths":["feature.txt"],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)}, nil
	}}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.RequiredMCPJSON = `[{"stage":"review","server":"cloudflare"}]`
	prober := &fakeMCPReadinessProber{result: types.MCPProbeResult{Status: types.MCPStatusAuthorizationRequiredDuringProbe}}
	var executor *pipeline.Executor
	gateCount := 0
	executor = pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&ReviewStep{}}, func(event ipc.Event) {
		if event.Type != ipc.EventStepCompleted || event.Status == nil || *event.Status != string(types.StepStatusAwaitingApproval) {
			return
		}
		gateCount++
		if agentCalls != 0 || prober.calls != 1 {
			t.Errorf("authorization gate launched work: agent calls %d, probes %d", agentCalls, prober.calls)
		}
		prober.result.Status = types.MCPStatusAuthorized
		if err := executor.Respond(types.StepReview, types.ActionFix, nil); err != nil {
			t.Error(err)
		}
	})
	executor.SetMCPReadinessProber(prober)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := executor.Execute(ctx, sctx.Run, sctx.Repo, dir); err != nil {
		t.Fatal(err)
	}
	if gateCount != 1 || prober.calls != 3 || agentCalls != 1 {
		t.Fatalf("gates=%d probes=%d agent calls=%d, want parked preflight then one authorized attempt", gateCount, prober.calls, agentCalls)
	}
}

func (p *fakeMCPReadinessProber) ProbeMCP(_ context.Context, cwd, server string) (types.MCPProbeResult, error) {
	p.calls++
	p.server = server
	p.cwd = cwd
	return p.result, p.err
}

func TestMCPPreflight_UndeclaredCloudflareDoesNotBlockUnrelatedWork(t *testing.T) {
	prober := &fakeMCPReadinessProber{}
	var logs []string
	sctx := &pipeline.StepContext{
		Ctx:       context.Background(),
		Run:       &db.Run{RequiredMCPJSON: ""},
		WorkDir:   "/run/worktree",
		MCPProber: prober,
		Log:       func(line string) { logs = append(logs, line) },
	}

	outcome, parked, err := mcpPreflightOutcome(sctx, types.StepReview)
	if err != nil || parked || outcome != nil {
		t.Fatalf("preflight = (%+v, %v, %v), want unrelated stage to continue", outcome, parked, err)
	}
	if prober.calls != 0 {
		t.Fatalf("probe calls = %d, want no Cloudflare probe without a declaration", prober.calls)
	}
	if got := strings.Join(logs, "\n"); !strings.Contains(got, types.MCPStatusDisabled) {
		t.Fatalf("logs = %q, want safe disabled outcome", got)
	}
}

func TestMCPPreflight_MissingAuthorizationReturnsTypedGate(t *testing.T) {
	requirement, err := types.ParseMCPRequirement("review:cloudflare")
	if err != nil {
		t.Fatal(err)
	}
	requiredJSON, err := types.MarshalMCPRequirements([]types.MCPRequirement{requirement})
	if err != nil {
		t.Fatal(err)
	}
	prober := &fakeMCPReadinessProber{result: types.MCPProbeResult{
		Status:          types.MCPStatusAuthorizationRequiredDuringProbe,
		ExecutorContext: "daemon CODEX_HOME=/daemon/.codex",
		NextAction:      "CODEX_HOME='/daemon/.codex' codex mcp login cloudflare --no-browser",
	}}
	var logs []string
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
	sctx.Run.RequiredMCPJSON = requiredJSON
	sctx.MCPProber = prober
	sctx.Log = func(line string) { logs = append(logs, line) }

	outcome, parked, err := mcpPreflightOutcome(sctx, types.StepReview)
	if err != nil || !parked || outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("preflight = (%+v, %v, %v), want authorization gate", outcome, parked, err)
	}
	if prober.calls != 1 || prober.server != "cloudflare" || prober.cwd != sctx.WorkDir {
		t.Fatalf("probe = calls:%d server:%q cwd:%q", prober.calls, prober.server, prober.cwd)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil || len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, err = %v", findings, err)
	}
	authorization := findings.Items[0].AuthorizationRequired
	if authorization == nil {
		t.Fatalf("finding = %+v, want typed authorization handoff", findings.Items[0])
	}
	if authorization.Provider != "cloudflare" || authorization.Server != "cloudflare" || authorization.Stage != "review" || authorization.Status != types.MCPStatusAuthorizationRequiredDuringProbe || authorization.ExecutorContext != "daemon CODEX_HOME=/daemon/.codex" || !strings.Contains(authorization.NextAction, "CODEX_HOME='/daemon/.codex'") {
		t.Fatalf("authorization handoff = %+v", authorization)
	}
	if !onlyMCPAuthorizationFindings(outcome.Findings) || !pipeline.HasMCPAuthorizationRefusal(outcome.Findings) {
		t.Fatalf("authorization gate was not recognized as a protected handoff: %s", outcome.Findings)
	}
	if strings.Contains(strings.Join(logs, "\n"), "token") || strings.Contains(outcome.Findings, "secret") {
		t.Fatalf("authorization result exposed secret-like data: logs=%q findings=%q", logs, outcome.Findings)
	}
}

func TestMCPPreflight_AuthorizedExactContextContinues(t *testing.T) {
	requirement, _ := types.ParseMCPRequirement("test:cloudflare")
	requiredJSON, _ := types.MarshalMCPRequirements([]types.MCPRequirement{requirement})
	prober := &fakeMCPReadinessProber{result: types.MCPProbeResult{
		Status:          types.MCPStatusAuthorized,
		ExecutorContext: "daemon CODEX_HOME=/daemon/.codex",
		NextAction:      "safe login action",
	}}
	sctx := &pipeline.StepContext{
		Ctx:       context.Background(),
		Run:       &db.Run{RequiredMCPJSON: requiredJSON},
		WorkDir:   "/run/worktree",
		MCPProber: prober,
		Log:       func(string) {},
	}

	outcome, parked, err := mcpPreflightOutcome(sctx, types.StepTest)
	if err != nil || parked || outcome != nil {
		t.Fatalf("preflight = (%+v, %v, %v), want authorized stage to continue", outcome, parked, err)
	}
	if prober.calls != 1 {
		t.Fatalf("probe calls = %d, want one authorization probe", prober.calls)
	}
}

func TestMCPToolAuthorizationFailureHasDistinctTypedOutcome(t *testing.T) {
	status, ok := mcpAuthorizationStatusFromError(&agent.MCPAuthorizationError{Server: "cloudflare"})
	if !ok || status.Status != types.MCPStatusAuthorizationRequiredDuringTool {
		t.Fatalf("tool-attempt status = %+v, matched = %v", status, ok)
	}
	sctx := &pipeline.StepContext{Ctx: context.Background(), Run: &db.Run{RequiredMCPJSON: `[{"stage":"test","server":"cloudflare"}]`}}
	outcome := mcpAuthorizationOutcome(sctx, types.StepTest, status, true)
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil || len(findings.Items) != 1 || findings.Items[0].AuthorizationRequired == nil {
		t.Fatalf("findings = %+v, err = %v", findings, err)
	}
	if findings.Items[0].AuthorizationRequired.Status != types.MCPStatusAuthorizationRequiredDuringTool {
		t.Fatalf("tool-attempt handoff = %+v", findings.Items[0].AuthorizationRequired)
	}
}

type mcpTestAgent struct{ *mockAgent }

func (a *mcpTestAgent) SupportsMCP(server, _ string) bool {
	return server == "cloudflare"
}

func TestTestStep_AuthorizedRetryDropsResolvedControls(t *testing.T) {
	for _, cut := range []string{"timeout", "process-exit"} {
		for _, selection := range []string{"authorization", "authorization-and-budget", "authorization-and-defect", "deferred-authorization"} {
			t.Run(cut+"/"+selection, func(t *testing.T) {
				dir, base, head := setupGitRepo(t)
				calls := 0
				ag := &mcpTestAgent{mockAgent: &mockAgent{name: "mcp", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					calls++
					if opts.Purpose == "test-fix" {
						if strings.Contains(opts.Prompt, "RESOLVED_AUTH_CONTROL") {
							t.Fatal("repair received resolved authorization")
						}
						return &agent.Result{Output: json.RawMessage(`{"summary":"fix defect"}`)}, nil
					}
					if cut == "process-exit" {
						return nil, &exec.ExitError{}
					}
					<-ctx.Done()
					return nil, ctx.Err()
				}}}
				sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
				sctx.Run.RequiredMCPJSON = `[{"stage":"test","server":"cloudflare"}]`
				prober := &fakeMCPReadinessProber{result: types.MCPProbeResult{Status: types.MCPStatusDeclaredNotAuthorized}}
				sctx.MCPProber = prober
				first, err := (&TestStep{}).Execute(sctx)
				if err != nil || first == nil || !pipeline.HasMCPAuthorizationRefusal(first.Findings) || calls != 0 {
					t.Fatalf("initial gate=%+v err=%v calls=%d", first, err, calls)
				}
				prober.result.Status = types.MCPStatusAuthorized
				sctx.Config.TestAgentTimeout = 20 * time.Millisecond
				sctx.Fixing = true
				findings, err := types.ParseFindingsJSON(first.Findings)
				if err != nil {
					t.Fatal(err)
				}
				findings.Items[0].Description = "RESOLVED_AUTH_CONTROL"
				if cut == "process-exit" {
					findings.Items[0].Category = ""
				} else {
					findings.Items[0].AuthorizationRequired = nil
				}
				if selection == "authorization-and-budget" {
					findings.Items = append(findings.Items, Finding{ID: types.FindingIDTestAgentTimeout, Severity: types.FindingSeverityWarning})
				}
				if selection == "authorization-and-defect" {
					findings.Items = append(findings.Items, Finding{ID: "defect", Severity: types.FindingSeverityError, Description: "retained defect"})
				}
				sctx.PreviousFindings, _ = types.MarshalFindingsJSON(findings)
				if selection == "deferred-authorization" {
					sctx.DeferredFindings = sctx.PreviousFindings
					sctx.PreviousFindings = `{"findings":[{"id":"test-agent-timeout","severity":"warning"}]}`
				}
				outcome, err := (&TestStep{}).Execute(sctx)
				if err != nil || outcome == nil || !outcome.NeedsApproval {
					t.Fatalf("retry=%+v err=%v", outcome, err)
				}
				expected := 1
				if selection == "authorization-and-defect" {
					expected = 2
				}
				if calls != expected || pipeline.HasMCPAuthorizationRefusal(outcome.Findings) || strings.Contains(outcome.Findings, "RESOLVED_AUTH_CONTROL") {
					t.Fatalf("calls=%d findings=%s", calls, outcome.Findings)
				}
				if selection == "authorization-and-defect" && !strings.Contains(outcome.Findings, "retained defect") {
					t.Fatalf("defect lost: %s", outcome.Findings)
				}
			})
		}
	}
}
