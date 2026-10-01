# Computer use protocol v2 (breaking)

`pkg/agentsdk/tools/computeruse` holds the protocol v2 wire types for the
`computer_use` tool and its desktop relay (`ProtocolVersion = 2`):

- `Action` — one tool input with snake_case fields (`action`, `coordinate`,
  `start_coordinate`, `text`, `repeat`, `scroll_direction`, `scroll_amount`,
  `duration`, `region`, `url`). `ActionNames` lists the accepted actions;
  `wait` runs in the agent tool and is never relayed.
- `Result` — the desktop answer (`requestId`, `ok`, `error`, `denied`,
  `screenshot`, `cursor`). Successful results always carry a `Screenshot`
  (base64 `image/jpeg` or `image/png` with its pixel size).

Coordinates are pixels of the most recent screenshot, origin top-left. The
model sees each screenshot as an image on the tool result. To bound context in
long sessions, set `RunConfig.MaxRetainedToolImages`: older tool-output images
are dropped in chunks of that size and replaced by
`[screenshot omitted to save context]`.

The v1 `selected_display` scope and observation metadata types are removed.
Protocol v2 is not compatible with v1: ship the desktop app, dashboard, and new
agent runs together. Validation rules and the full relay contract live in the
platform's `COMPUTER_USE.md`.
