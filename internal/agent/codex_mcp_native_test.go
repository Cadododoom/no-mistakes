package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Explicitly opt in: this checks the installed native CLI without inference,
// accounts, network servers, or the operator's Codex configuration.
func TestCodexMCPNativeSelectedProfile(t *testing.T) {
	if os.Getenv("NM_TEST_NATIVE_CODEX") != "1" {
		t.Skip("set NM_TEST_NATIVE_CODEX=1 to verify native app-server compatibility")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "codex-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[mcp_servers.cloudflare]\nenabled=false\ncommand=\"missing-base-command\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	profile := fmt.Sprintf("[mcp_servers.cloudflare]\nenabled=true\ncommand=%q\nargs=[\"-test.run=^TestCodexMCPNativeServerHelper$\"]\nenv={NM_NATIVE_MCP_HELPER=\"1\"}\n", helper)
	if err := os.WriteFile(filepath.Join(home, "connected.config.toml"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	environment := runenv.Overlay{Set: map[string]string{
		"HOME": dir, "CODEX_HOME": home, "XDG_CACHE_HOME": filepath.Join(dir, "cache"),
		"XDG_DATA_HOME": filepath.Join(dir, "data"), "XDG_STATE_HOME": filepath.Join(dir, "state"),
	}}
	for _, selectors := range [][]string{
		nil, {"-p", "connected"}, {"-p=connected"}, {"-pconnected"},
		{"--profile", "connected"}, {"--profile=connected"},
		{"-p", "connected", "-c", "mcp_servers.cloudflare.enabled=false"},
	} {
		t.Run(fmt.Sprint(selectors), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			result, err := NewCodexMCPProber(bin, selectors, environment).ProbeMCP(ctx, dir, "cloudflare")
			want := types.MCPStatusAuthorized
			if len(selectors) == 0 || len(selectors) == 4 {
				want = types.MCPStatusDeclaredNotAuthorized
			}
			if err != nil || result.Status != want {
				t.Fatalf("native readiness = %+v, %v; want %s", result, err, want)
			}
		})
	}
}

func TestCodexMCPNativeServerHelper(t *testing.T) {
	if os.Getenv("NM_NATIVE_MCP_HELPER") != "1" {
		t.Skip("MCP subprocess only")
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || request.ID == nil {
			continue
		}
		var result any = map[string]any{}
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "local-readiness", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "ready", "description": "Local readiness fixture", "inputSchema": map[string]any{"type": "object"}}}}
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}
	os.Exit(0)
}
