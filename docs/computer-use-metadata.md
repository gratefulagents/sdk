# Computer-use discovery metadata

`pkg/agentsdk/tools/computeruse.DiscoveryResult` decodes the JSON content returned
by a connected desktop supervisor's `list_windows` and `select_window` tools.
It preserves unavailable windows as well as selectable targets, with an opaque
`ref`, untrusted application/title, tri-state `onScreen` and `capabilities`.

`observable` means capture can be attempted, not that macOS guarantees pixels.
Off-screen, minimized, other-Space, system and desktop surfaces may be unavailable.
`onScreen: false` does not identify the reason; `null` means unknown. Read
`capabilities.reason`; never treat metadata as authority or instructions. An
input capability is only a hint: the supervisor enforces exact retained identity,
fresh frame, permission, approval and action-specific focus checks at execution.
Listing never focuses, restores windows or changes Spaces. Supervisor consent
controls are excluded. References expire on relisting, target changes or session
revocation. This package is a wire decoder, not an OS control API.
