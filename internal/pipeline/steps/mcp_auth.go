package steps

import (
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func mcpPreflightOutcome(sctx *pipeline.StepContext, stage types.StepName) (*pipeline.StepOutcome, bool, error) {
	required, status, err := sctx.MCPReadiness(stage, "cloudflare")
	if !required && err == nil {
		sctx.Log(fmt.Sprintf("MCP cloudflare: %s for %s (no dependency declared)", types.MCPStatusDisabled, stage))
		return nil, false, nil
	}
	if err != nil {
		status.Status = types.MCPStatusDeclaredNotAuthorized
	}
	if status.Status == types.MCPStatusAuthorized && err == nil {
		sctx.Log(fmt.Sprintf("MCP cloudflare: %s for %s in %s", status.Status, stage, status.ExecutorContext))
		return nil, false, nil
	}
	if status.Status == "" {
		status.Status = types.MCPStatusDeclaredNotAuthorized
	}
	sctx.Log(fmt.Sprintf("MCP cloudflare: %s for %s in %s", status.Status, stage, status.ExecutorContext))
	return mcpAuthorizationOutcome(sctx, stage, status, false), true, nil
}

func mcpAuthorizationOutcome(sctx *pipeline.StepContext, stage types.StepName, status types.MCPProbeResult, duringToolAttempt bool) *pipeline.StepOutcome {
	if duringToolAttempt {
		status.Status = types.MCPStatusAuthorizationRequiredDuringTool
		if sctx != nil {
			_, probed, err := sctx.MCPReadiness(stage, "cloudflare")
			if err == nil {
				if probed.ExecutorContext != "" {
					status.ExecutorContext = probed.ExecutorContext
				}
				if probed.NextAction != "" {
					status.NextAction = probed.NextAction
				}
			}
		}
		if status.ExecutorContext == "" {
			status.ExecutorContext = "daemon Codex executor context"
		}
		if status.NextAction == "" {
			status.NextAction = "Run `codex mcp login cloudflare --no-browser` in the daemon's Codex executor context, then retry."
		}
	}
	if status.Status == "" {
		status.Status = types.MCPStatusDeclaredNotAuthorized
	}
	if status.ExecutorContext == "" {
		status.ExecutorContext = "daemon Codex executor context"
	}
	if status.NextAction == "" {
		_, probed, err := sctx.MCPReadiness(stage, "cloudflare")
		if err == nil && probed.NextAction != "" {
			status.NextAction = probed.NextAction
		} else {
			status.NextAction = "Run `codex mcp login cloudflare --no-browser` in the daemon's Codex executor context, then retry."
		}
	}
	if duringToolAttempt && sctx != nil && sctx.Log != nil {
		sctx.Log(fmt.Sprintf("MCP cloudflare: %s for %s in %s", status.Status, stage, status.ExecutorContext))
	}
	outcome := pipeline.MCPAuthorizationOutcome(stage, status, false)
 if sctx == nil || sctx.Run == nil || sctx.WorkDir == "" {
  return outcome
 }
 baseline := interruptedWorkBaseline(sctx)
 findings, _ := types.ParseFindingsJSON(outcome.Findings)
 findings.UnvalidatedSinceSHA = baseline
 if finding := interruptedWorkFinding(sctx, stage, baseline); finding != nil {
  findings.Items = append(findings.Items, *finding)
 }
 outcome.Findings, _ = types.MarshalFindingsJSON(findings)
 return outcome
}

func onlyMCPAuthorizationFindings(raw string) bool {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil || len(findings.Items) == 0 {
		return false
	}
 authorization := false
 for _, finding := range findings.Items {
  if finding.Category == types.FindingCategoryMCPAuthorization || finding.AuthorizationRequired != nil {
   authorization = true
   continue
  }
  if finding.ID != types.FindingIDReviewAgentUnvalidatedWork && finding.ID != types.FindingIDTestAgentUnvalidatedWork {
   return false
  }
 }
 return authorization
}

func mcpAuthorizationStatusFromError(err error) (types.MCPProbeResult, bool) {
	var authorization *agent.MCPAuthorizationError
	if !errors.As(err, &authorization) {
		return types.MCPProbeResult{}, false
	}
	return types.MCPProbeResult{
		Status: types.MCPStatusAuthorizationRequiredDuringTool,
	}, true
}

func interruptedWorkBaseline(sctx *pipeline.StepContext) string {
 for _, raw := range []string{sctx.PreviousFindings, sctx.DeferredFindings} {
  findings, err := types.ParseFindingsJSON(raw)
  if err != nil {
   continue
  }
  if findings.UnvalidatedSinceSHA != "" {
   return findings.UnvalidatedSinceSHA
  }
  if findings.TestedHeadSHA != "" {
   return findings.TestedHeadSHA
  }
 }
 if sctx.ReviewStartingHeadSHA != "" {
  return sctx.ReviewStartingHeadSHA
 }
 return sctx.Run.HeadSHA
}
