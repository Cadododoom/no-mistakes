package steps

import (
	"context"
	"encoding/json"
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
	ag := &mockAgent{name: "mock", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		agentCalls++
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"reviewed_paths":["feature.txt"],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)}, nil
	}}
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
	sctx := &pipeline.StepContext{
		Ctx:       context.Background(),
		Run:       &db.Run{RequiredMCPJSON: requiredJSON},
		WorkDir:   "/run/worktree",
		MCPProber: prober,
		Log:       func(line string) { logs = append(logs, line) },
	}

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
