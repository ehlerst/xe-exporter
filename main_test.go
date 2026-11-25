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

func TestDiscoverVRAMsAndCollect(t *testing.T) {
	sample := "  use_type: 1\n  use_tt: 0\n  size: 25669140480\n  usage: 17741950976\ndefault_page_size: 4KiB\nman size:25669140480\n"

	t.Run("linux7_tile_vram_mm", func(t *testing.T) {
		dir := t.TempDir()
		tile0 := filepath.Join(dir, "tile0")
		tile1 := filepath.Join(dir, "tile1")
		if err := os.MkdirAll(tile0, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(tile1, 0755); err != nil {
			t.Fatal(err)
		}
		p0 := filepath.Join(tile0, "vram_mm")
		p1 := filepath.Join(tile1, "vram_mm")
		if err := os.WriteFile(p0, []byte(sample), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p1, []byte("  size: 12884901888\n  usage: 1048576\n"), 0644); err != nil {
			t.Fatal(err)
		}

		got := discoverVRAMs(dir)
		want := []VRAMSource{
			{ID: "vram0_mm", Path: p0},
			{ID: "vram1_mm", Path: p1},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d VRAM sources (%+v), want %d", len(got), got, len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("source[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}

		total, used := collectVRAM(got[0].Path)
		if total != 25669140480 || used != 17741950976 {
			t.Fatalf("collectVRAM(tile0/vram_mm) = (%d, %d), want (25669140480, 17741950976)", total, used)
		}
	})

	t.Run("legacy_vram_mm", func(t *testing.T) {
		dir := t.TempDir()
		p0 := filepath.Join(dir, "vram0_mm")
		if err := os.WriteFile(p0, []byte(sample), 0644); err != nil {
			t.Fatal(err)
		}

		got := discoverVRAMs(dir)
		if len(got) != 1 || got[0] != (VRAMSource{ID: "vram0_mm", Path: p0}) {
			t.Fatalf("got %+v, want single legacy vram0_mm", got)
		}
	})

	t.Run("deduplicate_when_both_exist", func(t *testing.T) {
		dir := t.TempDir()
		tile0 := filepath.Join(dir, "tile0")
		if err := os.MkdirAll(tile0, 0755); err != nil {
			t.Fatal(err)
		}
		tilePath := filepath.Join(tile0, "vram_mm")
		legacyPath0 := filepath.Join(dir, "vram0_mm")
		legacyPath1 := filepath.Join(dir, "vram1_mm")
		for _, p := range []string{tilePath, legacyPath0, legacyPath1} {
			if err := os.WriteFile(p, []byte(sample), 0644); err != nil {
				t.Fatal(err)
			}
		}

		got := discoverVRAMs(dir)
		want := []VRAMSource{
			{ID: "vram0_mm", Path: tilePath},
			{ID: "vram1_mm", Path: legacyPath1},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d VRAM sources (%+v), want %d", len(got), got, len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("source[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})
}
