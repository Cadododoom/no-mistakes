package pipeline

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const prelaunchMCPAuthorizationID = "prelaunch-mcp-authorization-required"

func MCPAuthorizationOutcome(stage types.StepName, status types.MCPProbeResult, prelaunch bool) *StepOutcome {
	if status.Status == "" {
		status.Status = types.MCPStatusDeclaredNotAuthorized
	}
	if status.ExecutorContext == "" {
		status.ExecutorContext = "daemon Codex executor context"
	}
	if status.NextAction == "" {
		status.NextAction = defaultMCPLoginAction("cloudflare")
	}
	description := fmt.Sprintf("The %s stage is parked because Cloudflare MCP authorization is required in the daemon Codex executor context. Authorize that context, then respond with fix to re-probe and resume the stage.", stage)
	id := types.FindingIDMCPAuthorizationRequired
	if prelaunch {
		id = prelaunchMCPAuthorizationID
		description = fmt.Sprintf("Pipeline execution is parked before any step runs because %s requires Cloudflare MCP authorization in the daemon Codex executor context. Authorize that context, then respond with fix to re-probe all declared prerequisites and start the pipeline.", stage)
	}
	payload, _ := types.MarshalFindingsJSON(types.Findings{
		Summary: "Required MCP authorization is unavailable",
		Items: []types.Finding{{
			ID:          id,
			Severity:    types.FindingSeverityWarning,
			Action:      types.ActionAskUser,
			Category:    types.FindingCategoryMCPAuthorization,
			Description: description,
			AuthorizationRequired: &types.MCPAuthorizationRequired{
				Provider:        "cloudflare",
				Server:          "cloudflare",
				Stage:           string(stage),
				Status:          status.Status,
				ExecutorContext: status.ExecutorContext,
				NextAction:      status.NextAction,
			},
		}},
	})
	return &StepOutcome{NeedsApproval: true, Findings: payload}
}

func (e *Executor) prelaunchMCPReadiness(sctx *StepContext) (*StepOutcome, error) {
	requirements, err := sctx.Run.MCPRequirements()
	if err != nil {
		return nil, err
	}
	for _, requirement := range requirements {
		_, status, err := sctx.MCPReadiness(requirement.Stage, requirement.Server)
		if err != nil {
			return nil, err
		}
		sctx.Log(fmt.Sprintf("Prelaunch MCP %s for %s: %s in %s", requirement.Server, requirement.Stage, status.Status, status.ExecutorContext))
		if status.Status != types.MCPStatusAuthorized {
			return MCPAuthorizationOutcome(requirement.Stage, status, true), nil
		}
	}
	return nil, nil
}
