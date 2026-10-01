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
model sees each screenshot as an image on the tool result. Screenshots must not
be downscaled or re-encoded after capture: their pixels define the coordinate
system for subsequent actions. Native capture handles the geometry.

The runner keeps exactly the three most recent image attachments (or fewer when
fewer are available), counting both message and tool-output images. Older images
are replaced with a text placeholder; checkpoints, snapshots, and transcripts
persist placeholders rather than base64 image payloads. `RunConfig.MaxRetainedToolImages`
remains for source compatibility but no longer controls retention. SDK hosts can
use `ElideOldImages(items, DefaultMaxRecentImages)` for the same context policy and
`StripImagesForPersistence(items)` before storing run items.

The v1 `selected_display` scope and observation metadata types are removed.
Protocol v2 is not compatible with v1: ship the desktop app, dashboard, and new
agent runs together. Validation rules and the full relay contract live in the
platform's `COMPUTER_USE.md`.
