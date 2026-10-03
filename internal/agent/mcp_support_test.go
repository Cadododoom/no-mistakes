package agent

import (
 "testing"

 "github.com/kunchenguid/no-mistakes/internal/types"
)

func TestMCPRequirementsValidateEveryDispatchCandidate(t *testing.T) {
 codex := func() Agent { return WithSteering(&codexAgent{}, "") }
 claude := func() Agent { return WithSteering(&claudeAgent{}, "") }
 cases := []struct {
  name string
  ag Agent
  reviewOK, testOK bool
 }{
  {"codex", codex(), true, true},
  {"claude", claude(), false, false},
  {"fallback", NewFallback([]Agent{codex(), claude()}), false, false},
  {"codex-fallback", NewFallback([]Agent{codex(), codex()}), true, true},
  {"codex-review-roles", WithReviewAgents(claude(), codex(), codex()), true, false},
  {"claude-fixer", WithReviewAgents(codex(), codex(), claude()), false, true},
  {"late-reviewer", WithReviewRoles(codex(), ReviewRoles{Reviewer: RoundedRole{Agent: codex(), Late: claude(), LateFrom: 2}}), false, true},
  {"late-fixer", WithReviewRoles(codex(), ReviewRoles{Fixer: RoundedRole{Agent: codex(), Late: claude(), LateFrom: 2}}), false, true},
  {"late-only-with-unsupported-base", WithReviewRoles(claude(), ReviewRoles{Reviewer: RoundedRole{Late: codex(), LateFrom: 2}, Fixer: RoundedRole{Agent: codex()}}), false, false},
 }
 for _, tc := range cases {
  t.Run(tc.name, func(t *testing.T) {
   for _, stage := range []types.StepName{types.StepReview, types.StepTest} {
    wantOK := tc.reviewOK
    if stage == types.StepTest { wantOK = tc.testOK }
    err := ValidateMCPRequirements(tc.ag, []types.MCPRequirement{{Stage: stage, Server: "cloudflare"}})
    if (err == nil) != wantOK { t.Fatalf("stage=%s err=%v wantOK=%v", stage, err, wantOK) }
   }
   if err := ValidateMCPRequirements(tc.ag, nil); err != nil { t.Fatalf("optional MCP blocked ordinary work: %v", err) }
  })
 }
}
