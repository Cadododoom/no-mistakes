package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// runPushBranch returns the remote branch the run publishes its head to and
// targets its pull request at: the explicit per-run binding recorded with
// `axi run --push-branch` when present, else the run's own branch. Run
// identity (gate mirror ref, rerun head recovery, prior-run lookup) stays on
// Run.Branch; only remote publication and PR head selection use this value.
func runPushBranch(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.Run == nil {
		return ""
	}
	return runPublishBranch(sctx.Run)
}

// runPublishBranch resolves the effective publish branch of a stored run.
func runPublishBranch(run *db.Run) string {
	if run == nil {
		return ""
	}
	if run.PushBranch != nil {
		if pushBranch := strings.TrimSpace(*run.PushBranch); pushBranch != "" {
			return pushBranch
		}
	}
	return strings.TrimPrefix(strings.TrimSpace(run.Branch), "refs/heads/")
}

// ValidateRunPushBranchName validates a per-run publish/PR head branch
// (`axi run --push-branch`). It accepts only the name; remote existence is not
// required - the binding can create a new remote branch or update an existing
// pull request's source branch.
func ValidateRunPushBranchName(branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", nil
	}
	if _, err := evidence.NormalizeBranch(branch); err != nil {
		return "", fmt.Errorf("invalid branch name: %w", err)
	}
	return branch, nil
}
