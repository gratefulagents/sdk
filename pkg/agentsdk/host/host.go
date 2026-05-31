// Package host exposes the SDK host-boundary interfaces used by ChatLoop.
package host

import "github.com/gratefulagents/sdk/pkg/agentsdk"

type Cursor = agentsdk.Cursor
type UserMessage = agentsdk.UserMessage
type WorkingState = agentsdk.WorkingState
type ToolApprovalRequest = agentsdk.ToolApprovalRequest
type MCPServerConfig = agentsdk.MCPServerConfig
type PermissionMode = agentsdk.PermissionMode

type SessionStore = agentsdk.SessionStore
type RunStatusSink = agentsdk.RunStatusSink
type ConfigSource = agentsdk.ConfigSource
type TraceStore = agentsdk.TraceStore
type ApprovalGate = agentsdk.ApprovalGate
type PlatformToolFactory = agentsdk.PlatformToolFactory
