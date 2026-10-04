package types

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// MCPRequirement declares an MCP server that one pipeline stage must be able
// to authorize in the daemon's Codex executor context before it starts.
type MCPRequirement struct {
	Stage  StepName `json:"stage"`
	Server string   `json:"server"`
}

// ParseMCPRequirement parses the public stage:server spelling used by
// `axi run --require-mcp` and Git push options.
func ParseMCPRequirement(raw string) (MCPRequirement, error) {
	stageText, server, ok := strings.Cut(strings.TrimSpace(raw), ":")
	if !ok || strings.Contains(server, ":") {
		return MCPRequirement{}, fmt.Errorf("MCP requirement %q must be stage:server", raw)
	}
	requirement := MCPRequirement{Stage: StepName(strings.TrimSpace(stageText)), Server: strings.TrimSpace(server)}
	if err := requirement.Validate(); err != nil {
		return MCPRequirement{}, err
	}
	return requirement, nil
}

// Validate checks the current Codex-MCP dependency contract. Review and Test
// are the agent stages whose incidental Cloudflare connection is gated today.
func (r MCPRequirement) Validate() error {
	if r.Stage != StepReview && r.Stage != StepTest {
		return fmt.Errorf("MCP requirement stage %q must be review or test", r.Stage)
	}
	if r.Server != "cloudflare" {
		return fmt.Errorf("MCP server %q is not supported yet; supported server: cloudflare", r.Server)
	}
	return nil
}

// ValidateMCPRequirements validates a complete immutable per-run requirement
// list and rejects duplicates.
func ValidateMCPRequirements(requirements []MCPRequirement) error {
	seen := make(map[string]struct{}, len(requirements))
	for _, requirement := range requirements {
		if err := requirement.Validate(); err != nil {
			return err
		}
		key := string(requirement.Stage) + ":" + requirement.Server
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate MCP requirement %q", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// CanonicalMCPRequirements validates and returns a stable, whitespace-trimmed
// ordering suitable for an immutable run record and launch proof.
func CanonicalMCPRequirements(requirements []MCPRequirement) ([]MCPRequirement, error) {
	canonical := append([]MCPRequirement(nil), requirements...)
	for i := range canonical {
		canonical[i].Server = strings.TrimSpace(canonical[i].Server)
	}
	if err := ValidateMCPRequirements(canonical); err != nil {
		return nil, err
	}
	slices.SortFunc(canonical, func(a, b MCPRequirement) int {
		if a.Stage < b.Stage {
			return -1
		}
		if a.Stage > b.Stage {
			return 1
		}
		return strings.Compare(a.Server, b.Server)
	})
	return canonical, nil
}

func MarshalMCPRequirements(requirements []MCPRequirement) (string, error) {
	canonical, err := CanonicalMCPRequirements(requirements)
	if err != nil {
		return "", err
	}
	if len(canonical) == 0 {
		return "", nil
	}
	payload, err := json.Marshal(canonical)
	return string(payload), err
}

func ParseMCPRequirements(payload string) ([]MCPRequirement, error) {
	if strings.TrimSpace(payload) == "" {
		return nil, nil
	}
	var requirements []MCPRequirement
	if err := json.Unmarshal([]byte(payload), &requirements); err != nil {
		return nil, fmt.Errorf("decode stored MCP requirements: %w", err)
	}
	return CanonicalMCPRequirements(requirements)
}

const (
	MCPStatusDisabled                         = "disabled"
	MCPStatusDeclaredNotAuthorized            = "declared_not_authorized"
	MCPStatusAuthorizationRequiredDuringProbe = "authorization_required_during_probe"
	MCPStatusAuthorizationRequiredDuringTool  = "authorization_required_during_tool_attempt"
	MCPStatusAuthorized                       = "authorized"
)

// MCPProbeResult contains only the authorization state and safe instructions.
// It never includes MCP server output, headers, prompts, or credentials.
type MCPProbeResult struct {
	Status          string `json:"status"`
	ExecutorContext string `json:"executor_context,omitempty"`
	NextAction      string `json:"next_action,omitempty"`
}
