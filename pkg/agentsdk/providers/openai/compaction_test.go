package openai

import "testing"

func TestCompactionDefaultsFromModelMetadataUsesCodexLimit(t *testing.T) {
	trigger, target, ok := CompactionDefaultsFromModelMetadata(ModelMetadata{
		ID:               "gpt-5.5",
		ContextWindow:    272000,
		MaxContextWindow: 272000,
	})
	if !ok {
		t.Fatal("expected metadata to produce compaction defaults")
	}
	if trigger != 244800 {
		t.Fatalf("trigger = %d, want 244800", trigger)
	}
	if target != 136000 {
		t.Fatalf("target = %d, want 136000", target)
	}
}

func TestCompactionDefaultsFromModelMetadataUsesCodexGPT56Limit(t *testing.T) {
	trigger, target, ok := CompactionDefaultsFromModelMetadata(ModelMetadata{
		ID:               "gpt-5.6-sol",
		ContextWindow:    372000,
		MaxContextWindow: 372000,
	})
	if !ok {
		t.Fatal("expected metadata to produce compaction defaults")
	}
	if trigger != 334800 {
		t.Fatalf("trigger = %d, want 334800", trigger)
	}
	if target != 186000 {
		t.Fatalf("target = %d, want 186000", target)
	}
}

func TestCompactionDefaultsFromModelMetadataClampsConfiguredLimit(t *testing.T) {
	trigger, target, ok := CompactionDefaultsFromModelMetadata(ModelMetadata{
		ID:                    "gpt-test",
		ContextWindow:         1000,
		AutoCompactTokenLimit: 950,
	})
	if !ok {
		t.Fatal("expected metadata to produce compaction defaults")
	}
	if trigger != 900 {
		t.Fatalf("trigger = %d, want 900", trigger)
	}
	if target != 500 {
		t.Fatalf("target = %d, want 500", target)
	}
}
