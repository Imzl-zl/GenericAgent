package domain

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultMCPTimeoutSeconds = 30
	MaxMCPTimeoutSeconds     = 300
)

var (
	ErrMCPServerNotFound = errors.New("MCP server not found")
	ErrMCPServerConflict = errors.New("MCP server conflict")
)

var mcpServerKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

type MCPServerCreate struct {
	ServerKey      string
	Name           string
	URL            string
	TimeoutSeconds int
}

type MCPServerUpdate struct {
	MCPServerCreate
}

type MCPServer struct {
	ID             int64
	ServerKey      string
	Name           string
	URL            string
	TimeoutSeconds int
	Enabled        bool
	Revision       int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func ValidateMCPServerInput(input MCPServerCreate) error {
	if !mcpServerKeyPattern.MatchString(strings.TrimSpace(input.ServerKey)) {
		return fmt.Errorf("server_key must contain 1-32 letters, digits, or underscores")
	}
	if strings.TrimSpace(input.Name) == "" {
		return fmt.Errorf("name is required")
	}
	parsed, err := url.Parse(strings.TrimSpace(input.URL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("url must be an absolute http or https URL")
	}
	if parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
		return fmt.Errorf("url must contain no credentials or fragment")
	}
	if input.TimeoutSeconds <= 0 || input.TimeoutSeconds > MaxMCPTimeoutSeconds {
		return fmt.Errorf("timeout_seconds must be between 1 and %d", MaxMCPTimeoutSeconds)
	}
	return nil
}
