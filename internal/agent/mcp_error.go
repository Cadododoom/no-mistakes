package agent

import (
	"errors"
	"strings"
)

// MCPAuthorizationError is returned only when a declared MCP dependency fails
// an authorization check during a Codex tool attempt. Its text contains no
// server response or credential material.
type MCPAuthorizationError struct {
	Server string
}

func (e *MCPAuthorizationError) Error() string {
	if e == nil || strings.TrimSpace(e.Server) == "" {
		return "required MCP server authorization is missing"
	}
	return "required MCP server " + e.Server + " requires authorization"
}

func IsMCPAuthorizationError(err error) bool {
	var target *MCPAuthorizationError
	return errors.As(err, &target)
}
