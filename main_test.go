package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollectHwmonTemperatures(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "temp1_input"), []byte("54250\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "temp1_label"), []byte("pkg\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "temp2_input"), []byte("61000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "temp2_label"), []byte("vram\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Missing label should fall back to sensor name (temp3).
	if err := os.WriteFile(filepath.Join(dir, "temp3_input"), []byte("47500\n"), 0644); err != nil {
		t.Fatal(err)
	}

	got := collectHwmonTemperatures(dir)
	if len(got) != 3 {
		t.Fatalf("expected 3 temperature readings, got %d", len(got))
	}
	want := []hwmonReading{
		{label: "pkg", value: 54.25},
		{label: "vram", value: 61.0},
		{label: "temp3", value: 47.5},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("reading[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestCollectHwmonPowerLimits(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "power1_cap"), []byte("220000000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "power1_crit"), []byte("440000000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "power1_label"), []byte("card\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Unlabeled fallback should strip "power" prefix -> "2".
	if err := os.WriteFile(filepath.Join(dir, "power2_cap"), []byte("150000000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	caps := collectHwmonPowerLimits(dir, "_cap")
	if len(caps) != 2 || caps[0] != (hwmonReading{label: "card", value: 220}) || caps[1] != (hwmonReading{label: "2", value: 150}) {
		t.Fatalf("unexpected caps: %+v", caps)
	}

	crits := collectHwmonPowerLimits(dir, "_crit")
	if len(crits) != 1 || crits[0] != (hwmonReading{label: "card", value: 440}) {
		t.Fatalf("unexpected crits: %+v", crits)
	}
}

func TestPMUParsers(t *testing.T) {
	ev, err := parsePMUEventID("event=0x02\n")
	if err != nil || ev != 2 {
		t.Fatalf("parsePMUEventID = (%d, %v), want (2, nil)", ev, err)
	}
	shift, err := parsePMUConfigShift("config:60-63\n")
	if err != nil || shift != 60 {
		t.Fatalf("parsePMUConfigShift = (%d, %v), want (60, nil)", shift, err)
	}
	f := &pmuFormat{
		activeEvent:   2,
		totalEvent:    3,
		eventShift:    0,
		instanceShift: 12,
		classShift:    20,
		gtShift:       60,
	}
	got := f.config(f.activeEvent, 1, 2, 1)
	want := uint64(2) | (uint64(1) << 12) | (uint64(2) << 20) | (uint64(1) << 60)
	if got != want {
		t.Fatalf("config = 0x%x, want 0x%x", got, want)
	}
}

func TestIsDRMCardDir(t *testing.T) {
	if !isDRMCardDir("card0") || !isDRMCardDir("card1") || !isDRMCardDir("card12") {
		t.Fatal("expected valid card dirs to pass")
	}
	if isDRMCardDir("card1-DP-1") || isDRMCardDir("card") || isDRMCardDir("renderD128") {
		t.Fatal("expected non-card dirs to fail")
	}
}
