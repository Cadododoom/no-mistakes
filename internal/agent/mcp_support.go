package agent

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func SupportsMCP(a Agent, server, purpose string) bool {
	support, ok := a.(interface{ SupportsMCP(string, string) bool })
	return ok && support.SupportsMCP(server, purpose)
}

func ValidateMCPRequirements(a Agent, requirements []types.MCPRequirement) error {
	if err := types.ValidateMCPRequirements(requirements); err != nil {
		return err
	}
	for _, requirement := range requirements {
		purposes := []string{"test"}
		if requirement.Stage == types.StepReview {
			purposes = []string{"review", "review-coverage", "review-fix"}
		}
		for _, purpose := range purposes {
			if err := RequireMCPServers(a, []string{requirement.Server}, purpose); err != nil {
				return err
			}
		}
	}
	return nil
}

func RequireMCPServers(a Agent, servers []string, purpose string) error {
	for _, server := range servers {
		if !SupportsMCP(a, server, purpose) {
			return fmt.Errorf("required MCP server %q is unsupported by an adapter configured for %s; only Codex supports declared Cloudflare requirements", server, purpose)
		}
	}
	return nil
}

func (a *codexAgent) SupportsMCP(server, _ string) bool {
	return server == "cloudflare"
}

func (a *fallbackAgent) SupportsMCP(server, purpose string) bool {
	if len(a.agents) == 0 {
		return false
	}
	for _, candidate := range a.agents {
		if !SupportsMCP(candidate, server, purpose) {
			return false
		}
	}
	return true
}

func (s steeredAgent) SupportsMCP(server, purpose string) bool {
	return SupportsMCP(s.Agent, server, purpose)
}

func (a *reviewAgents) SupportsMCP(server, purpose string) bool {
	candidates := []Agent{a.primary}
	switch purpose {
	case "review", "review-coverage":
		candidates = a.reviewer.agents()
		if a.reviewer.Agent == nil {
			candidates = append(candidates, a.primary)
		}
	case "review-fix":
		candidates = a.fixAgents()
	}
	for _, candidate := range candidates {
		if !SupportsMCP(candidate, server, purpose) {
			return false
		}
	}
	return len(candidates) > 0
}
