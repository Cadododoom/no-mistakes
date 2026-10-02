package steps

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

const agentExitTestCodeEnv = "NO_MISTAKES_AGENT_EXIT_TEST_CODE"

func simulatedAgentProcessExit(t *testing.T, code int) error {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAgentProcessExitHelper$")
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", agentExitTestCodeEnv, code))
	return cmd.Run()
}

func TestAgentProcessExitHelper(t *testing.T) {
	raw := os.Getenv(agentExitTestCodeEnv)
	if raw == "" {
		return
	}
	code, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(code)
}
