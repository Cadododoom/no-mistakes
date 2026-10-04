package daemon

import (
	"context"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPipelineMCPRequirementsUseResolvedAdapters(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       config.Config
		stage     types.StepName
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
			if err != nil {
				t.Fatal(err)
			}
			defer ag.Close()
			err = agent.ValidateMCPRequirements(ag, []types.MCPRequirement{{Stage: tc.stage, Server: "cloudflare"}})
			if (err == nil) != tc.supported {
				t.Fatalf("resolved adapter validation=%v supported=%v", err, tc.supported)
			}
		})
	}
}

func TestMCPIncompatibleLaunchPreservesActiveRun(t *testing.T) {
	for _, selection := range []struct {
		name, yaml string
		stage      types.StepName
	}{
		{"primary", "agent: claude\n", types.StepTest},
		{"trusted-primary", "agent: claude\n", types.StepTest},
		{"fallback", "agent: [codex, claude]\n", types.StepTest},
		{"reviewer", "agent: codex\nreview_agents:\n  reviewer: {agent: claude}\n", types.StepReview},
		{"fixer", "agent: codex\nreview_agents:\n  fixer: {agent: claude}\n", types.StepReview},
		{"later-reviewer", "agent: codex\nreview_agents:\n  reviewer_after_round: {agent: claude}\n", types.StepReview},
		{"later-fixer", "agent: codex\nreview_agents:\n  fixer_after_round: {agent: claude}\n", types.StepReview},
	} {
		for _, launch := range []string{"explicit", "fresh", "push", "push-nonce", "rerun"} {
			t.Run(selection.name+"/"+launch, func(t *testing.T) {
				p := paths.WithRoot(t.TempDir())
				if err := p.EnsureDirs(); err != nil {
					t.Fatal(err)
				}
				database, err := db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer database.Close()
				repo, head := setupTestGitRepo(t, p, database, "mcp-launch")
				if selection.name == "trusted-primary" {
					head = commitDefaultBranchConfig(t, repo.WorkingPath, selection.yaml)
				}
				active, err := database.InsertRun(repo.ID, "main", head, head)
				if err != nil {
					t.Fatal(err)
				}
				fake := writeMockClaude(t, t.TempDir())
				configSelection := selection.yaml
				if selection.name == "trusted-primary" {
					configSelection = "agent: codex\n"
				}
				yaml := configSelection + "agent_path_override:\n  codex: " + fake + "\n  claude: " + fake + "\n"
				if err := os.WriteFile(p.ConfigFile(), []byte(yaml), 0600); err != nil {
					t.Fatal(err)
				}
				manager := NewRunManager(database, p, nil)
				cancelled := false
				manager.cancels[active.ID] = func(error) { cancelled = true }
				requirements := []types.MCPRequirement{{Stage: selection.stage, Server: "cloudflare"}}
				ctx := context.Background()
				switch launch {
				case "explicit":
					_, err = manager.startRunWithMCP(ctx, repo, "main", head, head, "test", nil, "test", "", false, "", nil, requirements)
				case "fresh":
					_, err = manager.HandleStartFreshRun(ctx, &ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "main", HeadSHA: head, Intent: "test", LaunchNonce: "nonce", ValidationGeneration: "generation", RequiredMCP: requirements})
				case "push", "push-nonce":
					params := &ipc.PushReceivedParams{Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", Old: head, New: head, Intent: "test", RequiredMCP: requirements}
					if launch == "push-nonce" {
						params.LaunchNonce = "nonce"
						params.ValidationGeneration = "generation"
					}
					_, err = manager.HandlePushReceived(ctx, params)
				case "rerun":
					_, err = manager.HandleRerunWithMCP(ctx, repo.ID, "main", active.ID, nil, "test", "", false, head, "", nil, requirements)
				}
				if err == nil || !strings.Contains(err.Error(), "unsupported") {
					t.Fatalf("launch error = %v", err)
				}
				runs, err := database.GetRunsByRepo(repo.ID)
				if err != nil {
					t.Fatal(err)
				}
				if cancelled || len(runs) != 1 || runs[0].ID != active.ID {
					t.Fatalf("replacement disturbed active run: cancelled=%v runs=%v", cancelled, runs)
				}
			})
		}
	}
}
