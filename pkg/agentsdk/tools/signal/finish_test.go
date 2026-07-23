package signal

import "testing"

func TestFinishToolIsAvailableToReadOnlyRuns(t *testing.T) {
	tool := &FinishTool{}
	if !tool.IsReadOnly() {
		t.Fatal("finish must be classified as read-only lifecycle control")
	}
}
