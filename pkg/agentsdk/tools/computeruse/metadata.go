// Package computeruse defines discovery metadata returned by a connected desktop supervisor.
package computeruse

// WindowCapabilities describes current eligibility, not an authorization grant.
// Observable permits attempting capture; macOS can still refuse protected or off-screen content.
// Input always requires fresh identity, frame, permission, approval and action-specific focus checks.
type WindowCapabilities struct {
	Selectable bool   `json:"selectable"`
	Observable bool   `json:"observable"`
	Input      bool   `json:"input"`
	Reason     string `json:"reason"`
}

// WindowMetadata is untrusted display data. Ref expires with the discovery/session.
// OnScreen is nil when unknown; false does not identify a minimized window or another Space.
type WindowMetadata struct {
	Ref          string             `json:"ref"`
	Application  string             `json:"application"`
	Title        string             `json:"title"`
	OnScreen     *bool              `json:"onScreen"`
	Capabilities WindowCapabilities `json:"capabilities"`
}

// DiscoveryResult preserves the metadata in list_windows and select_window results.
type DiscoveryResult struct {
	Windows        []WindowMetadata `json:"windows,omitempty"`
	Target         *WindowMetadata  `json:"target,omitempty"`
	TargetRevision uint64           `json:"targetRevision"`
	Guidance       string           `json:"guidance"`
}
