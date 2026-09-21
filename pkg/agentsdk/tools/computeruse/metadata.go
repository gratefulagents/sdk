// Package computeruse describes the selected-display desktop control contract.
package computeruse

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const Mode = "selected_display"
const AttachOperation = "attach_desktop"

// DesktopScope binds locally selected display capture, not keyboard focus.
// Keyboard input is desktop-wide and follows OS focus, including other displays.
type DesktopScope struct {
	Mode      string `json:"mode"`
	Backend   string `json:"backend"`
	User      string `json:"user"`
	Namespace string `json:"namespace"`
	Run       string `json:"run"`
	DisplayID uint32 `json:"displayId"`
}

var ErrMigration = errors.New("legacy or invalid desktop scope: update desktop/backend/run and reconnect with selected-display capture and desktop-wide input consent")

func DecodeScope(raw []byte) (DesktopScope, error) {
	var scope DesktopScope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&scope) != nil || decoder.Decode(new(any)) != io.EOF || scope.Mode != Mode || scope.DisplayID == 0 || scope.Backend == "" || scope.User == "" || scope.Namespace == "" || scope.Run == "" {
		return DesktopScope{}, ErrMigration
	}
	return scope, nil
}

// ObservationResult contains vision analysis, never raw screenshots. FrameID is
// single-use for input; coordinates are returned PNG pixels, not desktop points.
type ObservationResult struct {
	ActionStatus string `json:"actionStatus,omitempty"`
	Analysis     string `json:"analysis"`
	FrameID      string `json:"frameId"`
	PixelWidth   int    `json:"pixelWidth"`
	PixelHeight  int    `json:"pixelHeight"`
	Coordinates  string `json:"coordinates"`
}
