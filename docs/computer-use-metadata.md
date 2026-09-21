# Selected-display desktop control (breaking contract)

`pkg/agentsdk/tools/computeruse` now describes `selected_display` scopes and
observation results. Window discovery metadata is removed. Connect only with
`attach_desktop` after a local display selection and an explicit **Start desktop
control** action authorizing capture and desktop-wide input. Separate consent
checkboxes are not required. Missing modes, old window fields, `attach` and
`attach_agent` must not be upgraded. Update all peers and reconnect.

Pointer coordinates are PNG pixels mapped into the chosen display. Keyboard
input follows OS focus, including other displays; this is not window isolation.
Every input needs the latest fresh observation. The backend tool owns the
observe/action/follow-up workflow; this SDK package is metadata, not an OS input
implementation. Raw screenshots are not returned in ObservationResult.

Release the SDK metadata change together with platform desktop/backend/run
changes. Consumers of removed WindowMetadata/DiscoveryResult types must migrate.
