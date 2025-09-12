package zoneawareness

import "testing"

func TestZoneawarenessReady(t *testing.T) {
	za := Zoneawareness{HasSynced: false}
	if za.Ready() {
		t.Errorf("Expected Ready() to be false when HasSynced is false")
	}

	za.HasSynced = true
	if !za.Ready() {
		t.Errorf("Expected Ready() to be true when HasSynced is true")
	}
}
