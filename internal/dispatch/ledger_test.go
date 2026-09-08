package dispatch

import (
	"path/filepath"
	"testing"
	"time"
)

func openTestLedger(t *testing.T) *Ledger {
	t.Helper()
	ledger, err := OpenLedger(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() {
		if err := ledger.Close(); err != nil {
			t.Errorf("close ledger: %v", err)
		}
	})
	return ledger
}

func TestLedgerKeepsPassesUntilResolved(t *testing.T) {
	ledger := openTestLedger(t)
	at := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	if err := ledger.RecordPass("hestia", "u1", PassSpawn, at); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RecordPass("hestia", "u2", PassMaintenance, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RecordPass("argus", "u3", PassSpawn, at); err != nil {
		t.Fatal(err)
	}

	passes, err := ledger.PendingPasses("hestia")
	if err != nil {
		t.Fatal(err)
	}
	if len(passes) != 2 {
		t.Fatalf("pending passes %+v, want two", passes)
	}
	if passes[0].Kind != PassSpawn || passes[0].Unit != "u1" || !passes[0].At.Equal(at) {
		t.Fatalf("first pending pass %+v, want the spawn pass at %s", passes[0], at)
	}

	if err := ledger.ResolvePass("u1", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	passes, err = ledger.PendingPasses("hestia")
	if err != nil {
		t.Fatal(err)
	}
	if len(passes) != 1 || passes[0].Unit != "u2" {
		t.Fatalf("pending passes after resolve %+v, want only u2", passes)
	}
}

func TestLedgerReportsTheNewestPass(t *testing.T) {
	ledger := openTestLedger(t)
	at := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	if _, ok, err := ledger.LastPass("hestia"); ok || err != nil {
		t.Fatalf("LastPass on an empty ledger = %v, %v; want false, nil", ok, err)
	}
	if err := ledger.RecordPass("hestia", "u1", PassSpawn, at); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RecordPass("hestia", "u2", PassMaintenance, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.ResolvePass("u2", at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	last, ok, err := ledger.LastPass("hestia")
	if err != nil || !ok {
		t.Fatalf("LastPass = %v, %v; want a pass", ok, err)
	}
	if last.Unit != "u2" || !last.At.Equal(at.Add(time.Minute)) {
		t.Fatalf("LastPass = %+v, want the resolved newer pass", last)
	}
}

func TestLedgerResolvesTaskUnitsAndStops(t *testing.T) {
	ledger := openTestLedger(t)
	at := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	if _, ok := ledger.UnitForTask("t_1"); ok {
		t.Fatal("UnitForTask found a task the ledger never saw")
	}
	if err := ledger.RecordSpawn("hestia", "u1", "t_1", at); err != nil {
		t.Fatal(err)
	}
	unit, ok := ledger.UnitForTask("t_1")
	if !ok || unit != "u1" {
		t.Fatalf("UnitForTask(t_1) = %q, %v; want u1", unit, ok)
	}
	if err := ledger.RecordStop("t_1", "u1", reasonMaxRuntime, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RecordSpawn("hestia", "u2", "t_1", at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	unit, _ = ledger.UnitForTask("t_1")
	if unit != "u2" {
		t.Fatalf("UnitForTask(t_1) = %q after a second spawn, want u2", unit)
	}
}

func TestLedgerKeepsOffsets(t *testing.T) {
	ledger := openTestLedger(t)

	off, err := ledger.Offset("runs/hestia.jsonl")
	if err != nil || off != 0 {
		t.Fatalf("Offset of an unseen key = %d, %v; want 0, nil", off, err)
	}
	if err := ledger.SetOffset("runs/hestia.jsonl", 4096); err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetOffset("runs/hestia.jsonl", 8192); err != nil {
		t.Fatal(err)
	}
	off, err = ledger.Offset("runs/hestia.jsonl")
	if err != nil || off != 8192 {
		t.Fatalf("Offset = %d, %v; want 8192", off, err)
	}
}

func TestLedgerNoticesOnce(t *testing.T) {
	ledger := openTestLedger(t)
	at := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	first, err := ledger.NoticeOnce(noticeNoMaxRuntime, "t_1", at)
	if err != nil || !first {
		t.Fatalf("first notice = %v, %v; want true, nil", first, err)
	}
	again, err := ledger.NoticeOnce(noticeNoMaxRuntime, "t_1", at.Add(time.Hour))
	if err != nil || again {
		t.Fatalf("second notice = %v, %v; want false, nil", again, err)
	}
	other, err := ledger.NoticeOnce(noticeNoMaxRuntime, "t_2", at)
	if err != nil || !other {
		t.Fatalf("notice for another card = %v, %v; want true, nil", other, err)
	}
}

func TestLedgerRemembersAStoppedRun(t *testing.T) {
	ledger := openTestLedger(t)
	at := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	stopped, err := ledger.Stopped("t_1", "u1")
	if err != nil || stopped {
		t.Fatalf("Stopped before any stop = %v, %v; want false, nil", stopped, err)
	}
	if err := ledger.RecordStop("t_1", "u1", reasonMaxRuntime, at); err != nil {
		t.Fatal(err)
	}
	stopped, err = ledger.Stopped("t_1", "u1")
	if err != nil || !stopped {
		t.Fatalf("Stopped after the stop = %v, %v; want true, nil", stopped, err)
	}
	if again, err := ledger.Stopped("t_1", "u2"); err != nil || again {
		t.Fatalf("Stopped for a later run of the same card = %v, %v; want false, nil", again, err)
	}
}
