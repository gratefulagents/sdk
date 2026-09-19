package computeruse

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestDiscoveryResultPreservesCapabilities(t *testing.T) {
	data, err := os.ReadFile("testdata/window-discovery.json")
	if err != nil {
		t.Fatal(err)
	}
	var result DiscoveryResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Windows) != 8 || result.Target.OnScreen == nil || *result.Target.OnScreen || result.Target.Capabilities.Input {
		t.Fatal("off-screen target lost")
	}
	if result.Windows[5].OnScreen != nil || result.Windows[5].Capabilities.Selectable {
		t.Fatal("unknown/unavailable target lost")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var before, after any
	json.Unmarshal(data, &before)
	json.Unmarshal(encoded, &after)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("typed SDK round trip dropped discovery metadata")
	}
}
