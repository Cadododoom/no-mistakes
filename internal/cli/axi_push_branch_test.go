package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFormatPushBranchPushOption(t *testing.T) {
	opt := formatPushBranchPushOption("stacked-pr")
	if opt != "no-mistakes.push-branch=stacked-pr" {
		t.Fatalf("formatPushBranchPushOption = %q", opt)
	}
	if got := formatPushBranchPushOption("   "); got != "" {
		t.Fatalf("blank push branch = %q, want empty", got)
	}
}

func TestParsePushBranchPushOptions(t *testing.T) {
	got, err := parsePushBranchPushOptions([]string{
		"no-mistakes.push-branch=first",
		"no-mistakes.skip=review",
		"no-mistakes.push-branch=stacked-pr",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "stacked-pr" {
		t.Fatalf("parsePushBranchPushOptions = %q, want last value stacked-pr", got)
	}
	if _, err := parsePushBranchPushOptions([]string{"no-mistakes.push-branch=  "}); err == nil {
		t.Fatal("empty push-branch push option must fail closed")
	}
}

func TestConflictingActiveRunPushBranch_AllowsMatchingOrOmitted(t *testing.T) {
	t.Parallel()
	stored := "stacked-pr"
	run := &ipc.RunInfo{ID: "run-1", PushBranch: &stored}
	if err := conflictingActiveRunPushBranch(run, "stacked-pr"); err != nil {
		t.Fatalf("matching --push-branch should reattach: %v", err)
	}
	if err := conflictingActiveRunPushBranch(run, ""); err != nil {
		t.Fatalf("omitting --push-branch should reattach: %v", err)
	}
	if err := conflictingActiveRunPushBranch(nil, "stacked-pr"); err != nil {
		t.Fatalf("no active run means no conflict: %v", err)
	}
}

func TestConflictingActiveRunPushBranch_RefusesMismatch(t *testing.T) {
	t.Parallel()
	stored := "other-pr"
	run := &ipc.RunInfo{ID: "run-1", PushBranch: &stored}
	err := conflictingActiveRunPushBranch(run, "stacked-pr")
	if err == nil {
		t.Fatal("expected conflict when --push-branch differs from the active run")
	}
	if !strings.Contains(err.Error(), "run-1") || !strings.Contains(err.Error(), "other-pr") {
		t.Fatalf("error = %v, want it to name the run and stored publish branch", err)
	}
}

func TestConflictingActiveRunPushBranch_RefusesWhenActiveRunHasNoBinding(t *testing.T) {
	t.Parallel()
	run := &ipc.RunInfo{ID: "run-1"}
	err := conflictingActiveRunPushBranch(run, "stacked-pr")
	if err == nil {
		t.Fatal("expected conflict when reattaching would discard --push-branch")
	}
	if !strings.Contains(err.Error(), "run-1") {
		t.Fatalf("error = %v, want it to name the active run", err)
	}
}

func TestRerunParams_CarriesPushBranch(t *testing.T) {
	t.Parallel()
	params := rerunParams("repo-1", "feature/x", []types.StepName{types.StepReview}, "user goal", "develop", "stacked-pr")
	if params.PushBranch != "stacked-pr" {
		t.Fatalf("rerunParams.PushBranch = %q, want stacked-pr", params.PushBranch)
	}
	if params.PRBaseBranch != "develop" {
		t.Fatalf("rerunParams.PRBaseBranch = %q, want develop", params.PRBaseBranch)
	}
}
