// Package policy loads the immutable capability policy manifest used by API and scheduler.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const schemaVersion = "genericagent.capability-policy.v1"

// ToolPolicy is one allowlisted tool set under a capability.
type ToolPolicy struct {
	Version      string
	AllowedTools []string
}

// Registry is an immutable capability/tool-policy map with a content digest.
type Registry interface {
	Digest() string
	Resolve(capabilityVersion, toolPolicyVersion string) (ToolPolicy, error)
}

type registry struct {
	digest       string
	capabilities map[string]map[string]ToolPolicy
}

// LoadRegistry parses only genericagent.capability-policy.v1 from path.
// Digest is sha256:<lowercase-hex> over the exact file bytes. No compiled fallback.
func LoadRegistry(path string) (Registry, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("policy path is empty")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy file: %w", err)
	}
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	var root struct {
		SchemaVersion string `json:"schema_version"`
		Capabilities  map[string]struct {
			ToolPolicies map[string]struct {
				AllowedTools []string `json:"allowed_tools"`
			} `json:"tool_policies"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("invalid policy JSON: %w", err)
	}
	if root.SchemaVersion != schemaVersion {
		return nil, fmt.Errorf("unsupported schema_version %q (want %s)", root.SchemaVersion, schemaVersion)
	}
	if len(root.Capabilities) == 0 {
		return nil, fmt.Errorf("capabilities must be a non-empty object")
	}

	caps := make(map[string]map[string]ToolPolicy, len(root.Capabilities))
	seenPolicy := make(map[string]struct{})
	for capName, capBody := range root.Capabilities {
		if strings.TrimSpace(capName) == "" {
			return nil, fmt.Errorf("capability name must be non-empty")
		}
		if len(capBody.ToolPolicies) == 0 {
			return nil, fmt.Errorf("capability %q needs non-empty tool_policies", capName)
		}
		polMap := make(map[string]ToolPolicy, len(capBody.ToolPolicies))
		for polName, polBody := range capBody.ToolPolicies {
			if strings.TrimSpace(polName) == "" {
				return nil, fmt.Errorf("tool_policy version must be non-empty")
			}
			if _, dup := seenPolicy[polName]; dup {
				return nil, fmt.Errorf("duplicate tool_policy version %q", polName)
			}
			seenPolicy[polName] = struct{}{}
			if len(polBody.AllowedTools) == 0 {
				return nil, fmt.Errorf("tool_policy %q allowed_tools must be non-empty", polName)
			}
			tools := make([]string, 0, len(polBody.AllowedTools))
			seenTool := make(map[string]struct{}, len(polBody.AllowedTools))
			for _, t := range polBody.AllowedTools {
				if strings.TrimSpace(t) == "" {
					return nil, fmt.Errorf("tool_policy %q has empty tool name", polName)
				}
				if _, d := seenTool[t]; d {
					return nil, fmt.Errorf("tool_policy %q has duplicate tool %q", polName, t)
				}
				seenTool[t] = struct{}{}
				tools = append(tools, t)
			}
			polMap[polName] = ToolPolicy{Version: polName, AllowedTools: tools}
		}
		caps[capName] = polMap
	}
	return &registry{digest: digest, capabilities: caps}, nil
}

func (r *registry) Digest() string { return r.digest }

func (r *registry) Resolve(capabilityVersion, toolPolicyVersion string) (ToolPolicy, error) {
	polMap, ok := r.capabilities[capabilityVersion]
	if !ok {
		return ToolPolicy{}, fmt.Errorf("unknown capability_version: %q", capabilityVersion)
	}
	pol, ok := polMap[toolPolicyVersion]
	if !ok {
		for otherCap, otherMap := range r.capabilities {
			if otherCap != capabilityVersion {
				if _, found := otherMap[toolPolicyVersion]; found {
					return ToolPolicy{}, fmt.Errorf(
						"tool_policy_version %q belongs to capability %q, not %q",
						toolPolicyVersion, otherCap, capabilityVersion,
					)
				}
			}
		}
		return ToolPolicy{}, fmt.Errorf("unknown tool_policy_version: %q", toolPolicyVersion)
	}
	// Defensive copy of AllowedTools.
	out := ToolPolicy{Version: pol.Version, AllowedTools: append([]string(nil), pol.AllowedTools...)}
	return out, nil
}
