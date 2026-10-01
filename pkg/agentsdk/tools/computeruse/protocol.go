// Package computeruse defines the computer-use protocol v2 wire types shared
// by the agent tool, the relay broker, and the desktop app.
//
// Coordinates are pixels of the most recent screenshot, origin top-left.
// Every successful action returns a fresh screenshot taken after the screen
// settles. Validation of action fields lives with the tool, not here.
package computeruse

// ProtocolVersion is the relay protocol version. v2 is not compatible with v1.
const ProtocolVersion = 2

// ActionNames lists every action accepted by the computer_use tool. "wait" is
// executed by the agent tool itself and never relayed to the desktop.
var ActionNames = []string{
	"screenshot",
	"left_click",
	"right_click",
	"middle_click",
	"double_click",
	"triple_click",
	"mouse_move",
	"left_click_drag",
	"left_mouse_down",
	"left_mouse_up",
	"scroll",
	"type",
	"key",
	"wait",
	"cursor_position",
	"zoom",
	"open_url",
}

// Action is one computer_use tool input, relayed to the desktop as-is.
// Fields that do not belong to Action.Action must be left zero.
type Action struct {
	Action string `json:"action"`
	// Coordinate is [x, y]; for left_click_drag it is the end point.
	Coordinate      []int `json:"coordinate,omitempty"`
	StartCoordinate []int `json:"start_coordinate,omitempty"`
	// Text is typed text, a key chord, or held modifiers for clicks and scroll.
	Text string `json:"text,omitempty"`
	// Repeat is the key press count (1–50).
	Repeat int `json:"repeat,omitempty"`
	// ScrollDirection is up, down, left, or right.
	ScrollDirection string `json:"scroll_direction,omitempty"`
	// ScrollAmount is 1–30; zero means the default of 3.
	ScrollAmount int `json:"scroll_amount,omitempty"`
	// Duration is the wait in seconds (0.1–30); zero means the default of 1.
	Duration float64 `json:"duration,omitempty"`
	// Region is the zoom rectangle [x0, y0, x1, y1].
	Region []int `json:"region,omitempty"`
	// URL is an absolute http/https URL for open_url.
	URL string `json:"url,omitempty"`
}

// Screenshot is a base64-encoded image/jpeg or image/png frame whose
// dimensions match the image header.
type Screenshot struct {
	MediaType string `json:"mediaType"`
	Data      string `json:"data"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// Cursor is the pointer position in screenshot pixels.
type Cursor struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Result is the desktop's answer to a relayed request. When OK is false,
// Error is non-empty; Denied is set only when the user refused the action.
// Successful results always carry a Screenshot.
type Result struct {
	RequestID  string      `json:"requestId"`
	OK         bool        `json:"ok"`
	Error      string      `json:"error,omitempty"`
	Denied     bool        `json:"denied,omitempty"`
	Screenshot *Screenshot `json:"screenshot,omitempty"`
	Cursor     *Cursor     `json:"cursor,omitempty"`
}
