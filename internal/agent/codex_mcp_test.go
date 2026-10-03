package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCodexMCPProberRequiresAuthorizationInDaemonHome(t *testing.T) {
	const (
		foregroundHome = "/foreground/.codex"
		daemonHome     = "/daemon/.codex"
	)
	prober := NewCodexMCPProber("/opt/codex", []string{"-c", "mcp_servers.cloudflare.enabled=true"}, runenv.Overlay{
		Set: map[string]string{"CODEX_HOME": daemonHome},
	})
	prober.probe = func(_ context.Context, bin, cwd string, args, env []string) (types.MCPProbeResult, error) {
		if bin != "/opt/codex" || cwd != "/repo" {
			t.Fatalf("probe command = %q in %q, want configured binary and executor worktree", bin, cwd)
		}
		if strings.Join(args, " ") != "-c mcp_servers.cloudflare.enabled=true" {
			t.Fatalf("probe config args = %q", args)
		}
		home := envValue(env, "CODEX_HOME")
		if home == foregroundHome {
			return types.MCPProbeResult{Status: types.MCPStatusAuthorized}, nil
		}
		if home != daemonHome {
			t.Fatalf("probe CODEX_HOME = %q, want daemon executor home %q", home, daemonHome)
		}
		return types.MCPProbeResult{Status: types.MCPStatusAuthorizationRequiredDuringProbe}, nil
	}

	result, err := prober.ProbeMCP(context.Background(), "/repo", "cloudflare")
	if err != nil {
		t.Fatalf("ProbeMCP: %v", err)
	}
	if result.Status != types.MCPStatusAuthorizationRequiredDuringProbe {
		t.Fatalf("status = %q, want authorization required in daemon context", result.Status)
	}
	if result.ExecutorContext != "daemon CODEX_HOME="+daemonHome {
		t.Fatalf("executor context = %q", result.ExecutorContext)
	}
	if !strings.Contains(result.NextAction, "CODEX_HOME='"+daemonHome+"'") || !strings.Contains(result.NextAction, "'/opt/codex' mcp login 'cloudflare' --no-browser") || strings.Contains(result.NextAction, "mcp_servers.cloudflare.enabled=true") {
		t.Fatalf("next action = %q, want exact-context login command", result.NextAction)
	}
}

func TestCodexAgentCloudflareAvailableOnlyWhenDeclared(t *testing.T) {
	for _, resume := range []bool{false, true} {
		for _, declared := range []bool{false, true} {
			t.Run(fmt.Sprintf("resume=%v/declared=%v", resume, declared), func(t *testing.T) {
				dir := t.TempDir()
				capture := filepath.Join(dir, "args.txt")
				bin := writeFakeCodex(t, dir, `#!/bin/sh
if case "$*" in *"mcp list --json"*) true;; *) false;; esac; then printf '%s\n' '[{"name":"cloudflare"}]'; exit 0; fi
printf '%s\n' "$@" > "$CAPTURE_FILE"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`, "@echo off\r\necho %* | findstr /c:\"mcp list --json\" >nul && (echo [{\"name\":\"cloudflare\"}] & exit /b 0)\r\necho %*>\"%CAPTURE_FILE%\"\r\necho {\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"ok\"}}\r\necho {\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\r\n")
				ca := &codexAgent{bin: bin, extraArgs: []string{"-c", "mcp_servers.cloudflare.enabled=true"}, subprocessContext: newSubprocessContext(runenv.Overlay{Set: map[string]string{"CAPTURE_FILE": capture}})}
				opts := RunOpts{CWD: dir, Prompt: "test", ManageMCPAvailability: true}
				if declared {
					opts.RequiredMCPServers = []string{"cloudflare"}
				}
				if resume {
					opts.Session = &SessionRef{ID: "thread"}
				}
				if _, err := ca.Run(context.Background(), opts); err != nil {
					t.Fatal(err)
				}
				args, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(args), "mcp_servers.cloudflare.enabled=false") == declared {
					t.Fatalf("declaration=%v: unexpected Cloudflare availability in %s", declared, args)
				}
			})
		}
	}
}

func TestCodexMCPProbeRequiresRuntimeAuthorization(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake app-server uses a POSIX shell")
	}
	for _, test := range []struct{ runtime, auth, want string }{
		{"connected", "oAuth", types.MCPStatusAuthorized},
		{"authenticationRequired", "notLoggedIn", types.MCPStatusAuthorizationRequiredDuringProbe},
		{"failed", "oAuth", types.MCPStatusDeclaredNotAuthorized},
		{"disabled", "oAuth", types.MCPStatusDeclaredNotAuthorized},
		{"connected", "notLoggedIn", types.MCPStatusAuthorizationRequiredDuringProbe},
	} {
		t.Run(test.runtime+"/"+test.auth, func(t *testing.T) {
			dir := t.TempDir()
			bin := writeFakeCodex(t, dir, `#!/bin/sh
while IFS= read -r request; do
 case "$request" in
  *'"id":1,'*) printf '%s\n' '{"id":1,"result":{}}' ;;
  *'"id":2,'*) printf '%s\n' '{"id":2,"result":{"thread":{"id":"probe-thread"}}}' ;;
  *'"id":3,'*) printf '%s\n' '{"id":3,"result":{"data":[{"name":"cloudflare","runtimeStatus":"starting","authStatus":"unknown"}]}}' ;;
  *'"id":4,'*) printf '{"id":4,"result":{"data":[{"name":"cloudflare","runtimeStatus":"%s","authStatus":"%s"}]}}\n' "$PROBE_RUNTIME" "$PROBE_AUTH" ;;
 esac
done
`, "")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := runCodexMCPAppServer(ctx, bin, dir, nil, append(gitSafeEnv(dir), "PROBE_RUNTIME="+test.runtime, "PROBE_AUTH="+test.auth))
			if err != nil || result.Status != test.want {
				t.Fatalf("probe = %+v, %v; want %s", result, err, test.want)
			}
		})
	}
}

func TestCodexMCPExecutorHomeUsesEffectiveEnvironment(t *testing.T) {
	env := []string{"HOME=/executor-home"}
	if got := codexExecutorContext(env); got != "daemon Codex default home="+filepath.Join("/executor-home", ".codex") {
		t.Fatalf("executor context = %q", got)
	}
	if action := codexMCPLoginCommand("codex", env, "cloudflare", nil); !strings.Contains(action, "CODEX_HOME='"+filepath.Join("/executor-home", ".codex")+"'") {
		t.Fatalf("login action uses a different home: %s", action)
	}
}

func TestCodexMCPAbsentServerDoesNotCreateTransport(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCodex(t, dir, "#!/bin/sh\nprintf '%s\\n' '[]'\n", "@echo off\r\necho []\r\n")
	ca := &codexAgent{bin: bin}
	args, err := ca.mcpStageArgs(context.Background(), RunOpts{CWD: dir, ManageMCPAvailability: true})
	if err != nil || len(args) != 0 {
		t.Fatalf("absent server generated config: %q, %v", args, err)
	}
}

func TestCodexMCPInventoryErrorNeverExposesConfiguration(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCodex(t, dir, "#!/bin/sh\nprintf '%s\\n' 'private-token-must-not-leak' >&2\nexit 1\n", "@echo off\r\necho private-token-must-not-leak 1>&2\r\nexit /b 1\r\n")
	ca := &codexAgent{bin: bin}
	_, err := ca.mcpStageArgs(context.Background(), RunOpts{CWD: dir, ManageMCPAvailability: true})
	if err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("inventory failure exposed configuration: %v", err)
	}
}

func TestCodexMCPProberAcceptsAuthorizationInExactExecutorHome(t *testing.T) {
	const daemonHome = "/daemon/.codex"
	prober := NewCodexMCPProber("codex", nil, runenv.Overlay{Set: map[string]string{"CODEX_HOME": daemonHome}})
	prober.probe = func(_ context.Context, _, _ string, _, env []string) (types.MCPProbeResult, error) {
		if envValue(env, "CODEX_HOME") != daemonHome {
			t.Fatalf("probe did not receive daemon CODEX_HOME: %q", envValue(env, "CODEX_HOME"))
		}
		return types.MCPProbeResult{Status: types.MCPStatusAuthorized}, nil
	}

	result, err := prober.ProbeMCP(context.Background(), "/repo", "cloudflare")
	if err != nil {
		t.Fatalf("ProbeMCP: %v", err)
	}
	if result.Status != types.MCPStatusAuthorized || result.ExecutorContext != "daemon CODEX_HOME="+daemonHome {
		t.Fatalf("result = %+v, want authorization in the exact daemon home", result)
	}
}

func TestFailedRequiredMCPAuthorizationRequiresMatchingDeclaredServer(t *testing.T) {
	for _, test := range []struct {
		name     string
		server   string
		required []string
		want     string
		error    string
	}{
		{name: "declared", server: "cloudflare", required: []string{"cloudflare"}, want: "cloudflare"},
		{name: "undeclared", server: "cloudflare", required: nil},
		{name: "different server", server: "other", required: []string{"cloudflare"}},
		{name: "different configuration key", server: "Cloudflare", required: []string{"cloudflare"}},
		{name: "unaccepted requirement alias", server: "cloudflare", required: []string{"Cloudflare"}},
		{name: "unaccepted matching alias", server: "Cloudflare", required: []string{"Cloudflare"}},
		{name: "non authorization error", server: "cloudflare", required: []string{"cloudflare"}, error: `{"message":"temporary upstream failure"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := &codexItem{Type: "mcp_tool_call", Server: test.server, Error: []byte(`{"code":"AuthRequired","message":"authorization required"}`)}
			if test.error != "" {
				item.Error = []byte(test.error)
			}
			if got := failedRequiredMCPAuthorization(item, test.required); got != test.want {
				t.Fatalf("authorization server = %q, want %q", got, test.want)
			}
		})
	}
}

func envValue(env []string, key string) string {
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == key {
			return value
		}
	}
	return ""
}

func TestCodexMCPInventoryUsesGlobalProfileFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake inventory uses a POSIX shell")
	}
	dir := t.TempDir()
	bin := writeFakeCodex(t, dir, `#!/bin/sh
if [ "$1" != -p ] || [ "$2" != executor ] || [ "$3" != mcp ]; then exit 1; fi
printf '%s\n' '[{"name":"cloudflare"}]'
`, "")
	ca := &codexAgent{bin: bin, extraArgs: []string{"-p", "executor", "--model", "ignored"}}
	args, err := ca.mcpStageArgs(context.Background(), RunOpts{CWD: dir, ManageMCPAvailability: true})
	if err != nil || strings.Join(args, " ") != "-c mcp_servers.cloudflare.enabled=false" {
		t.Fatalf("profile inventory = %q, %v", args, err)
	}
}

func TestCodexMCPConfigIsolationIsNeverDropped(t *testing.T) {
	got := codexMCPConfigArgs([]string{"--ignore-user-config", "--profile=executor", "-c", "mcp_servers.cloudflare.enabled=true", "--model", "ignored"})
	want := "--ignore-user-config --profile=executor -c mcp_servers.cloudflare.enabled=true"
	if strings.Join(got, " ") != want {
		t.Fatalf("probe configuration = %q, want %q", got, want)
	}
}

func TestCodexMCPLoginHandoffExplainsPrivateConfiguration(t *testing.T) {
 for _, selectors := range [][]string{
  {"-c", `mcp_servers.cloudflare.url="https://different.example/mcp?token=private-secret"`},
  {"--profile", "private-profile-name"},
  {"--config=mcp_servers.cloudflare.http_headers.Authorization=private-secret"},
  {"--ignore-user-config"},
  {`-c=mcp_servers.cloudflare.url="https://different.example/mcp?token=private-secret"`},
  {`-cmcp_servers.cloudflare.http_headers.Authorization="private-secret"`},
  {"-p=private-profile-name"},
  {"-pprivate-profile-name"},
 } {
  prober := NewCodexMCPProber("codex", selectors, runenv.Overlay{Set: map[string]string{"CODEX_HOME": "/daemon/.codex"}})
  prober.probe = func(_ context.Context, _, _ string, args, _ []string) (types.MCPProbeResult, error) {
   if strings.Join(args, "\n") != strings.Join(selectors, "\n") {
    t.Fatalf("probe lost effective configuration: %q", args)
   }
   return types.MCPProbeResult{Status: types.MCPStatusAuthorizationRequiredDuringProbe}, nil
  }
  result, err := prober.ProbeMCP(context.Background(), "/repo", "cloudflare")
  if err != nil { t.Fatal(err) }
  if !strings.Contains(result.NextAction, "same trusted Codex configuration selectors") || !strings.Contains(result.NextAction, "apply them privately to the login command") || !strings.Contains(result.NextAction, "CODEX_HOME='/daemon/.codex'") {
   t.Fatalf("handoff lost configuration or home instructions: %s", result.NextAction)
  }
  for _, secret := range []string{"private-secret", "private-profile-name", "different.example"} {
   if strings.Contains(result.NextAction, secret) { t.Fatalf("handoff disclosed configuration: %s", result.NextAction) }
  }
 }
}

func TestCodexMCPInventoryRequiresExactConfigurationKey(t *testing.T) {
 for _, name := range []string{"cloudflare", "Cloudflare", "CLOUDFLARE"} {
  t.Run(name, func(t *testing.T) {
   dir := t.TempDir()
   bin := writeFakeCodex(t, dir, "#!/bin/sh\nprintf '%s\\n' '[{\"name\":\""+name+"\"}]'\n", "@echo off\r\necho [{\"name\":\""+name+"\"}]\r\n")
   ca := &codexAgent{bin: bin}
   args, err := ca.mcpStageArgs(context.Background(), RunOpts{CWD: dir, ManageMCPAvailability: true})
   if err != nil { t.Fatal(err) }
   if (len(args) != 0) != (name == "cloudflare") { t.Fatalf("inventory name=%s overrides=%v", name, args) }
  })
 }
}

func TestCodexMCPReadinessRequiresExactConfigurationKey(t *testing.T) {
 if runtime.GOOS == "windows" { t.Skip("fake app-server uses a POSIX shell") }
 dir := t.TempDir()
 bin := writeFakeCodex(t, dir, `#!/bin/sh
while IFS= read -r request; do
 case "$request" in
  *'"id":1,'*) printf '%s\n' '{"id":1,"result":{}}' ;;
  *'"id":2,'*) printf '%s\n' '{"id":2,"result":{"thread":{"id":"probe-thread"}}}' ;;
  *'"id":3,'*) printf '%s\n' '{"id":3,"result":{"data":[{"name":"Cloudflare","runtimeStatus":"connected","authStatus":"oAuth"}]}}' ;;
 esac
done
`, "")
 ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
 defer cancel()
 result, err := runCodexMCPAppServer(ctx, bin, dir, nil, gitSafeEnv(dir))
 if err != nil || result.Status != types.MCPStatusDeclaredNotAuthorized { t.Fatalf("readiness for an alternate key=%+v err=%v", result, err) }
}

func TestCodexMCPAttachedSelectorsUseExactExecutorConfiguration(t *testing.T) {
 if runtime.GOOS == "windows" { t.Skip("fake configuration-aware Codex uses a POSIX shell") }
 for _, tc := range []struct { name, selector, runtimeStatus, want string }{
  {"config-equals-disable", "-c=mcp_servers.cloudflare.enabled=false", "disabled", types.MCPStatusDeclaredNotAuthorized},
  {"config-attached-disable", "-cmcp_servers.cloudflare.enabled=false", "disabled", types.MCPStatusDeclaredNotAuthorized},
  {"config-equals-url", `-c=mcp_servers.cloudflare.url="https://private.example/mcp?token=private-secret"`, "authenticationRequired", types.MCPStatusAuthorizationRequiredDuringProbe},
  {"config-attached-url", `-cmcp_servers.cloudflare.url="https://private.example/mcp?token=private-secret"`, "authenticationRequired", types.MCPStatusAuthorizationRequiredDuringProbe},
  {"profile-equals", "-p=private-profile", "authenticationRequired", types.MCPStatusAuthorizationRequiredDuringProbe},
  {"profile-attached", "-pprivate-profile", "authenticationRequired", types.MCPStatusAuthorizationRequiredDuringProbe},
 } {
  for _, resume := range []bool{false, true} {
   t.Run(fmt.Sprintf("%s/resume=%t", tc.name, resume), func(t *testing.T) {
    dir := t.TempDir()
    capture := filepath.Join(dir, "argv")
    bin := writeFakeCodex(t, dir, `#!/bin/sh
selected=false
mode=inventory
for arg do
 if [ "$arg" = "$EXPECTED_SELECTOR" ]; then selected=true; fi
 case "$arg" in app-server) mode=probe;; exec) mode=exec;; esac
done
printf '%s\n' "$@" > "$CAPTURE_PREFIX.$mode"
case "$mode" in
 inventory)
  if "$selected"; then printf '%s\n' '[{"name":"cloudflare"}]'; else printf '%s\n' '[]'; fi
  exit 0 ;;
 exec)
  printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}'
  printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
  exit 0 ;;
esac
runtime=connected
if "$selected"; then runtime="$CONFIGURED_RUNTIME"; fi
while IFS= read -r request; do
 case "$request" in
  *'"id":1,'*) printf '%s\n' '{"id":1,"result":{}}' ;;
  *'"id":2,'*) printf '%s\n' '{"id":2,"result":{"thread":{"id":"probe-thread"}}}' ;;
  *'"id":3,'*) printf '{"id":3,"result":{"data":[{"name":"cloudflare","runtimeStatus":"%s","authStatus":"oAuth"}]}}\n' "$runtime" ;;
 esac
done
`, "")
    environment := runenv.Overlay{Set: map[string]string{
     "CAPTURE_PREFIX": capture,
     "EXPECTED_SELECTOR": tc.selector,
     "CONFIGURED_RUNTIME": tc.runtimeStatus,
     "CODEX_HOME": filepath.Join(dir, "executor-home"),
    }}
    extraArgs := []string{tc.selector, "--model", "unrelated-model"}
    prober := NewCodexMCPProber(bin, extraArgs, environment)
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    result, err := prober.ProbeMCP(ctx, dir, "cloudflare")
    if err != nil || result.Status != tc.want { t.Fatalf("runtime readiness=%+v err=%v want=%s", result, err, tc.want) }
    if !strings.Contains(result.NextAction, "apply them privately to the login command") { t.Fatalf("missing private configuration handoff: %s", result.NextAction) }
    for _, private := range []string{tc.selector, "private-secret", "private.example", "private-profile"} {
     if strings.Contains(result.NextAction, private) { t.Fatalf("login handoff exposed selector: %s", result.NextAction) }
    }
    ca := &codexAgent{bin: bin, extraArgs: extraArgs, subprocessContext: newSubprocessContext(environment)}
    overrides, err := ca.mcpStageArgs(ctx, RunOpts{CWD: dir, ManageMCPAvailability: true})
    if err != nil || strings.Join(overrides, " ") != "-c mcp_servers.cloudflare.enabled=false" { t.Fatalf("inventory used another configuration: %q err=%v", overrides, err) }
    opts := RunOpts{CWD: dir, Prompt: "test", ManageMCPAvailability: true, RequiredMCPServers: []string{"cloudflare"}}
    if resume { opts.Session = &SessionRef{ID: "thread"} }
    if _, err := ca.Run(ctx, opts); err != nil { t.Fatal(err) }
    for _, mode := range []string{"inventory", "probe", "exec"} {
     raw, err := os.ReadFile(capture+"."+mode)
     if err != nil { t.Fatal(err) }
     args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
     count := 0
     model := false
     for _, arg := range args {
      if arg == tc.selector { count++ }
      if arg == "unrelated-model" { model = true }
     }
     if count != 1 || model != (mode == "exec") { t.Fatalf("%s argv=%q; want one exact selector and model only for execution", mode, args) }
    }
   })
  }
 }
}
