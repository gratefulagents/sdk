package computeruse

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDisplayScopeRequiresExplicitMigration(t *testing.T) {
	valid := `{"mode":"selected_display","backend":"https://example.com","user":"u","namespace":"n","run":"r","displayId":42}`
	scope, err := DecodeScope([]byte(valid))
	if err != nil || scope.DisplayID != 42 {
		t.Fatal(scope, err)
	}
	for _, raw := range []string{`{}`, `{"mode":"selected_window","windowId":42}`, `{"mode":"agent_choice"}`, valid + ` {}`, valid[:len(valid)-1] + `,"windowId":42}`, `{"mode":"selected_display","displayId":0}`} {
		if _, err := DecodeScope([]byte(raw)); !errors.Is(err, ErrMigration) {
			t.Fatal(raw, err)
		}
	}
	if Mode != "selected_display" || AttachOperation != "attach_desktop" {
		t.Fatal("handshake drift")
	}
}
func TestObservationMetadataRoundTrip(t *testing.T) {
	raw := `{"actionStatus":"completed","analysis":"Visible dialog","frameId":"f","pixelWidth":1920,"pixelHeight":1080,"coordinates":"PNG pixels"}`
	var result ObservationResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result)
	if err != nil || string(data) != raw {
		t.Fatal(string(data), err)
	}
}
