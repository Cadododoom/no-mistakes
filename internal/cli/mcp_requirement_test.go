package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func TestAxiRunRequiredMCPRefusesDaemonWithoutReadinessBeforeLaunch(t *testing.T) {
	for _, declined := range []bool{false, true} {
		t.Run(fmt.Sprint(declined), func(t *testing.T) {
			launched := olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil }, func(server *ipc.Server) {
				if declined {
					server.Handle(ipc.MethodProbeMCPReadiness, func(context.Context, json.RawMessage) (interface{}, error) {
						return &ipc.ProbeMCPReadinessResult{OK: false}, nil
					})
				}
			})
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetOut(&out)
			err := runAxiRunWithMCPRequirements(cmd, false, nil, "goal", "", false, "", "", defaultAxiWait, []types.MCPRequirement{{Stage: types.StepTest, Server: "cloudflare"}})
			if err == nil || !strings.Contains(out.String(), "cannot enforce --require-mcp") || len(*launched) != 0 {
				t.Fatalf("unsupported daemon launched=%v err=%v output=%s", *launched, err, out.String())
			}
		})
	}
}

func TestMCPRequirementFlagsAndPushOptionsRoundTrip(t *testing.T) {
	want := []types.MCPRequirement{
		{Stage: types.StepReview, Server: "cloudflare"},
		{Stage: types.StepTest, Server: "cloudflare"},
	}
	got, err := parseMCPRequirementFlags([]string{"test:cloudflare", "review:Cloudflare"})
	if err != nil {
		t.Fatalf("parseMCPRequirementFlags: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requirements = %+v, want %+v", got, want)
	}
	options := formatMCPRequirementPushOptions(got)
	decoded, err := parseMCPRequirementPushOptions(options)
	if err != nil {
		t.Fatalf("parseMCPRequirementPushOptions: %v", err)
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("push option requirements = %+v, want %+v", decoded, want)
	}
}

func TestMCPRequirementFlagsRejectUnsupportedAndDuplicateDependencies(t *testing.T) {
	for _, values := range [][]string{
		{"lint:cloudflare"},
		{"review:other"},
		{"review:cloudflare", "review:Cloudflare"},
	} {
		if _, err := parseMCPRequirementFlags(values); err == nil {
			t.Fatalf("parseMCPRequirementFlags(%q) succeeded, want validation error", values)
		}
	}
}
