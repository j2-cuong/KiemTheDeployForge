package guiutil

import (
	"testing"
	"time"
)

// never throttles; always throttles unless a report is structural.
const (
	noThrottle     = time.Duration(0)
	alwaysThrottle = time.Hour
)

func TestRelayForwardsFirstReport(t *testing.T) {
	relay := NewRelay(alwaysThrottle)
	update, ok := relay.Next(0, "Scan and hash", "Game.exe")
	if !ok {
		t.Fatal("first report was dropped")
	}
	if !update.StageChanged || !update.DetailChanged {
		t.Fatalf("first report must report both changes: %+v", update)
	}
}

func TestRelayDropsUnchangedReports(t *testing.T) {
	relay := NewRelay(noThrottle)
	if _, ok := relay.Next(10, "Scan and hash", "Game.exe"); !ok {
		t.Fatal("first report was dropped")
	}
	if _, ok := relay.Next(10, "Scan and hash", "Game.exe"); ok {
		t.Fatal("an identical report reached the window")
	}
}

// A large payload reports once per I/O block with the same file name and a
// percentage that barely moves. Those are exactly the reports that made the
// window strobe, so they must be collapsed.
func TestRelayThrottlesRapidSameFileReports(t *testing.T) {
	relay := NewRelay(alwaysThrottle)
	if _, ok := relay.Next(30, "Build payload package", "payload/Client/big.pak"); !ok {
		t.Fatal("first report was dropped")
	}
	for percent := 31; percent <= 60; percent++ {
		if _, ok := relay.Next(percent, "Build payload package", "payload/Client/big.pak"); ok {
			t.Fatalf("report at %d%% was not throttled", percent)
		}
	}
}

func TestRelayNeverDropsStageChange(t *testing.T) {
	relay := NewRelay(alwaysThrottle)
	if _, ok := relay.Next(30, "Build payload package", "a"); !ok {
		t.Fatal("first report was dropped")
	}
	update, ok := relay.Next(31, "Build ISO UDF", "b")
	if !ok {
		t.Fatal("a stage change was throttled away")
	}
	if !update.StageChanged {
		t.Fatalf("stage change was not flagged: %+v", update)
	}
}

func TestRelayNeverDropsCompletion(t *testing.T) {
	relay := NewRelay(alwaysThrottle)
	if _, ok := relay.Next(30, "Complete", "x"); !ok {
		t.Fatal("first report was dropped")
	}
	if _, ok := relay.Next(100, "Complete", "y"); !ok {
		t.Fatal("the final report was throttled away")
	}
}

func TestRelayReportsDetailChangeSeparatelyFromPercent(t *testing.T) {
	relay := NewRelay(noThrottle)
	if _, ok := relay.Next(10, "Scan and hash", "one.pak"); !ok {
		t.Fatal("first report was dropped")
	}
	// Only the percentage moved, so the file name label must be left alone.
	update, ok := relay.Next(11, "Scan and hash", "one.pak")
	if !ok {
		t.Fatal("a percentage change was dropped")
	}
	if update.DetailChanged {
		t.Fatal("an unchanged file name was reported as changed")
	}
	update, ok = relay.Next(11, "Scan and hash", "two.pak")
	if !ok {
		t.Fatal("a file change was dropped")
	}
	if !update.DetailChanged || update.StageChanged {
		t.Fatalf("expected only a detail change: %+v", update)
	}
}
