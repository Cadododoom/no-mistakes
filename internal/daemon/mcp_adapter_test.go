package daemon

import (
 "context"
 "testing"

 "github.com/kunchenguid/no-mistakes/internal/agent"
 "github.com/kunchenguid/no-mistakes/internal/config"
 "github.com/kunchenguid/no-mistakes/internal/runenv"
 "github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPipelineMCPRequirementsUseResolvedAdapters(t *testing.T) {
 for _, tc := range []struct {
  name string
  cfg config.Config
  stage types.StepName
  supported bool
 }{
  {"codex", config.Config{Agent: types.AgentCodex}, types.StepTest, true},
  {"claude", config.Config{Agent: types.AgentClaude}, types.StepTest, false},
  {"fallback", config.Config{Agents: []types.AgentName{types.AgentCodex, types.AgentClaude}}, types.StepTest, false},
  {"review-fixer", config.Config{Agent: types.AgentCodex, ReviewAgents: map[string]config.ReviewAgent{config.RoleFixer: {Agent: types.AgentClaude}}}, types.StepReview, false},
  {"review-late", config.Config{Agent: types.AgentCodex, ReviewAgents: map[string]config.ReviewAgent{config.RoleReviewerAfterRound: {Agent: types.AgentClaude}}}, types.StepReview, false},
  {"review-roles", config.Config{Agent: types.AgentClaude, ReviewAgents: map[string]config.ReviewAgent{config.RoleReviewer: {Agent: types.AgentCodex}, config.RoleFixer: {Agent: types.AgentCodex}}}, types.StepReview, true},
 } {
  t.Run(tc.name, func(t *testing.T) {
   ag, err := newPipelineAgent(context.Background(), &tc.cfg, t.TempDir(), fakeLookPath, runenv.Overlay{})
   if err != nil { t.Fatal(err) }
   defer ag.Close()
   err = agent.ValidateMCPRequirements(ag, []types.MCPRequirement{{Stage: tc.stage, Server: "cloudflare"}})
   if (err == nil) != tc.supported { t.Fatalf("resolved adapter validation=%v supported=%v", err, tc.supported) }
  })
 }
}
