package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const mcpRequirementPushOptionPrefix = "no-mistakes.required-mcp="

func requireDaemonMCPReadiness(client *ipc.Client, requirements []types.MCPRequirement) error {
	if len(requirements) == 0 {
		return nil
	}
	var result ipc.ProbeMCPReadinessResult
	if err := client.Call(ipc.MethodProbeMCPReadiness, struct{}{}, &result); err != nil || !result.OK {
		return fmt.Errorf("the running daemon cannot enforce --require-mcp; have its operator update it before launching this run")
	}
	return nil
}

func parseMCPRequirementFlags(values []string) ([]types.MCPRequirement, error) {
	requirements := make([]types.MCPRequirement, 0, len(values))
	for _, value := range values {
		requirement, err := types.ParseMCPRequirement(value)
		if err != nil {
			return nil, err
		}
		requirements = append(requirements, requirement)
	}
	return types.CanonicalMCPRequirements(requirements)
}

func formatMCPRequirementPushOptions(requirements []types.MCPRequirement) []string {
	canonical, err := types.CanonicalMCPRequirements(requirements)
	if err != nil || len(canonical) == 0 {
		return nil
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return nil
	}
	return []string{mcpRequirementPushOptionPrefix + base64.RawURLEncoding.EncodeToString(payload)}
}

func parseMCPRequirementPushOptions(options []string) ([]types.MCPRequirement, error) {
	var requirements []types.MCPRequirement
	found := false
	for _, option := range options {
		raw, ok := strings.CutPrefix(option, mcpRequirementPushOptionPrefix)
		if !ok {
			continue
		}
		if found || len(raw) > 4096 {
			return nil, fmt.Errorf("invalid MCP requirement push option")
		}
		found = true
		payload, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid MCP requirement push option")
		}
		if err := json.Unmarshal(payload, &requirements); err != nil {
			return nil, fmt.Errorf("invalid MCP requirement push option")
		}
	}
	canonical, err := types.CanonicalMCPRequirements(requirements)
	if err != nil {
		return nil, fmt.Errorf("invalid MCP requirement push option: %w", err)
	}
	return canonical, nil
}

func sameMCPRequirements(a, b []types.MCPRequirement) bool {
	left, err := types.CanonicalMCPRequirements(a)
	if err != nil {
		return false
	}
	right, err := types.CanonicalMCPRequirements(b)
	if err != nil || len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
