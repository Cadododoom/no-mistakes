package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const codexMCPProbeTimeout = 20 * time.Second

// CodexMCPProber asks Codex app-server for runtime MCP status in an ephemeral
// thread. A configured entry alone never counts as authorized.
type CodexMCPProber struct {
	bin   string
	args  []string
	env   subprocessContext
	probe func(context.Context, string, string, []string, []string) (types.MCPProbeResult, error)
}

func NewCodexMCPProber(bin string, codexArgs []string, environment runenv.Overlay) *CodexMCPProber {
	if strings.TrimSpace(bin) == "" {
		bin = "codex"
	}
	return &CodexMCPProber{
		bin:   bin,
		args:  codexMCPConfigArgs(codexArgs),
		env:   newSubprocessContext(environment),
		probe: runCodexMCPAppServer,
	}
}

func (p *CodexMCPProber) ProbeMCP(ctx context.Context, cwd, server string) (types.MCPProbeResult, error) {
	if p == nil || p.probe == nil {
		return types.MCPProbeResult{Status: types.MCPStatusDeclaredNotAuthorized}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, codexMCPProbeTimeout)
	defer cancel()
	env := p.env.gitSafeEnv(cwd)
	result, err := p.probe(probeCtx, p.bin, cwd, p.args, env)
	result.ExecutorContext = codexExecutorContext(env)
	if result.NextAction == "" {
		result.NextAction = codexMCPLoginCommand(p.bin, env, server, p.args)
	}
	return result, err
}

func codexMCPConfigArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--ignore-user-config":
			// Preserve config isolation even on CLIs whose probe commands do not
			// support it: rejection must not authorize against a different config.
			out = append(out, arg)
		case "-c", "--config", "-p", "--profile":
			if i+1 < len(args) {
				out = append(out, arg, args[i+1])
				i++
			}
		default:
			if strings.HasPrefix(arg, "--config=") || strings.HasPrefix(arg, "--profile=") {
				out = append(out, arg)
			}
		}
	}
	return out
}

func runCodexMCPAppServer(ctx context.Context, bin, cwd string, configArgs, env []string) (types.MCPProbeResult, error) {
	result := types.MCPProbeResult{Status: types.MCPStatusDeclaredNotAuthorized}
	args := append(append([]string(nil), configArgs...), "app-server")
	args = append(args, "--listen", "stdio://")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stderr = io.Discard
	shellenv.ConfigureShellCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return result, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return result, err
	}
	if err := shellenv.StartShellCommand(cmd); err != nil {
		return result, err
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Cancel != nil {
			_ = cmd.Cancel()
		} else {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		shellenv.TerminateShellCommandGroup(cmd)
	}()

	reader := bufio.NewReaderSize(stdout, 64*1024)
	writer := bufio.NewWriter(stdin)
	if err := writeAppServerMessage(writer, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"clientInfo":   map[string]string{"name": "no-mistakes", "title": "no-mistakes MCP readiness", "version": "1"},
			"capabilities": map[string]any{},
		},
	}); err != nil {
		return result, err
	}
	if _, err := readAppServerResponse(ctx, reader, 1); err != nil {
		return result, err
	}
	if err := writeAppServerMessage(writer, map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}}); err != nil {
		return result, err
	}
	if err := writeAppServerMessage(writer, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "thread/start",
		"params": map[string]any{"cwd": cwd, "ephemeral": true},
	}); err != nil {
		return result, err
	}
	threadResponse, err := readAppServerResponse(ctx, reader, 2)
	if err != nil {
		return result, err
	}
	threadID := firstNonEmptyJSONPath(threadResponse, []string{"result", "thread", "id"}, []string{"result", "id"})
	if threadID == "" {
		return result, errors.New("Codex MCP probe did not start a thread")
	}
	for id := 3; ; id++ {
		if err := writeAppServerMessage(writer, map[string]any{
			"jsonrpc": "2.0", "id": id, "method": "mcpServerStatus/list",
			"params": map[string]any{"threadId": threadID, "serverName": "cloudflare", "detail": "toolsAndAuthOnly"},
		}); err != nil {
			return result, err
		}
		statusResponse, err := readAppServerResponse(ctx, reader, id)
		if err != nil {
			return result, err
		}
		status := findMCPServerStatus(statusResponse, "cloudflare")
		if status == nil {
			return result, nil
		}
		runtimeStatus := strings.ToLower(stringValue(status["runtimeStatus"]))
		authStatus := strings.ToLower(stringValue(status["authStatus"]))
		switch {
		case runtimeStatus == "authenticationrequired" || authStatus == "notloggedin":
			result.Status = types.MCPStatusAuthorizationRequiredDuringProbe
			return result, nil
		case runtimeStatus == "connected":
			result.Status = types.MCPStatusAuthorized
			return result, nil
		case runtimeStatus != "starting" && runtimeStatus != "notstarted":
			return result, nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
		}
	}
}

func writeAppServerMessage(writer *bufio.Writer, message any) error {
	if err := json.NewEncoder(writer).Encode(message); err != nil {
		return err
	}
	return writer.Flush()
}

func readAppServerResponse(ctx context.Context, reader *bufio.Reader, id int) (map[string]any, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var message map[string]any
		if json.Unmarshal(line, &message) != nil {
			continue
		}
		if number, ok := message["id"].(float64); !ok || int(number) != id {
			continue
		}
		if message["error"] != nil {
			return nil, errors.New("Codex MCP probe request failed")
		}
		return message, nil
	}
}

func firstNonEmptyJSONPath(value map[string]any, paths ...[]string) string {
	for _, path := range paths {
		var current any = value
		for _, key := range path {
			object, ok := current.(map[string]any)
			if !ok {
				current = nil
				break
			}
			current = object[key]
		}
		if id, ok := current.(string); ok && id != "" {
			return id
		}
	}
	return ""
}

func findMCPServerStatus(response map[string]any, wanted string) map[string]any {
	result, _ := response["result"].(map[string]any)
	data, _ := result["data"].([]any)
	for _, value := range data {
		status, _ := value.(map[string]any)
		if strings.EqualFold(stringValue(status["name"]), wanted) {
			return status
		}
	}
	return nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func codexExecutorContext(env []string) string {
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == "CODEX_HOME" && value != "" {
			return "daemon CODEX_HOME=" + filepath.Clean(value)
		}
	}
	home := codexHomeFromEnv(env)
	if home == "" {
		return "daemon Codex default home"
	}
	return "daemon Codex default home=" + home
}

func codexMCPLoginCommand(bin string, env []string, server string, configArgs []string) string {
	home := codexHomeFromEnv(env)
	command := shellQuote(bin) + " mcp login " + shellQuote(server) + " --no-browser"
	if home != "" {
		command = "CODEX_HOME=" + shellQuote(home) + " " + command
	}
 if len(configArgs) > 0 {
  command = "Before login, supply the same trusted Codex configuration selectors (-c/--config, -p/--profile, or --ignore-user-config) used by this executor. Their values are omitted for privacy; obtain them from the daemon operator and apply them privately to the login command. Do not share credential values. Base command: " + command
 }
 return command
}

func codexHomeFromEnv(env []string) string {
	var home string
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == "CODEX_HOME" {
			home = value
			break
		}
	}
	if home == "" {
		for _, entry := range env {
			key, value, ok := strings.Cut(entry, "=")
			if ok && key == "HOME" && value != "" {
				home = filepath.Join(value, ".codex")
				break
			}
		}
	}
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(userHome, ".codex")
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Clean(home)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
