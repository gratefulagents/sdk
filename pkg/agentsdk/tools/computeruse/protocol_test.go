package computeruse

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestActionJSONRoundTrip(t *testing.T) {
	cases := map[string]Action{
		`{"action":"screenshot"}`: {Action: "screenshot"},
		`{"action":"left_click_drag","coordinate":[30,40],"start_coordinate":[10,20],"text":"shift"}`: {
			Action: "left_click_drag", Coordinate: []int{30, 40}, StartCoordinate: []int{10, 20}, Text: "shift",
		},
		`{"action":"scroll","coordinate":[5,6],"scroll_direction":"down","scroll_amount":5}`: {
			Action: "scroll", Coordinate: []int{5, 6}, ScrollDirection: "down", ScrollAmount: 5,
		},
		`{"action":"key","text":"cmd+c","repeat":2}`:          {Action: "key", Text: "cmd+c", Repeat: 2},
		`{"action":"wait","duration":0.5}`:                    {Action: "wait", Duration: 0.5},
		`{"action":"zoom","region":[0,0,100,50]}`:             {Action: "zoom", Region: []int{0, 0, 100, 50}},
		`{"action":"open_url","url":"https://example.com/a"}`: {Action: "open_url", URL: "https://example.com/a"},
	}
	for wire, want := range cases {
		var got Action
		if err := json.Unmarshal([]byte(wire), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("decode %s = %+v, want %+v", wire, got, want)
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != wire {
			t.Fatalf("encode = %s, want %s", encoded, wire)
		}
	}
}

func TestResultJSONRoundTrip(t *testing.T) {
	cases := map[string]Result{
		`{"requestId":"r1","ok":true,"screenshot":{"mediaType":"image/jpeg","data":"AAAA","width":1183,"height":768},"cursor":{"x":10,"y":20}}`: {
			RequestID:  "r1",
			OK:         true,
			Screenshot: &Screenshot{MediaType: "image/jpeg", Data: "AAAA", Width: 1183, Height: 768},
			Cursor:     &Cursor{X: 10, Y: 20},
		},
		`{"requestId":"r2","ok":false,"error":"user denied the action","denied":true}`: {
			RequestID: "r2", Error: "user denied the action", Denied: true,
		},
	}
	for wire, want := range cases {
		var got Result
		if err := json.Unmarshal([]byte(wire), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("decode %s = %+v, want %+v", wire, got, want)
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != wire {
			t.Fatalf("encode = %s, want %s", encoded, wire)
		}
	}
}

func TestActionNamesUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range ActionNames {
		if seen[name] {
			t.Fatalf("duplicate action %q", name)
		}
		seen[name] = true
	}
	if len(ActionNames) != 17 {
		t.Fatalf("len(ActionNames) = %d, want 17", len(ActionNames))
	}
}
