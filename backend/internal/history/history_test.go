package history

import (
	"errors"
	"path/filepath"
	"testing"
)

const tick = 50

func TestGenerateSaveLoadRoundTrip(t *testing.T) {
	now := int64(1_790_000_000_000)
	f := Generate("BTCUSDT", 42, 3, now, 6_500_000, tick)
	if len(f.Candles) != 3*24*60 {
		t.Fatalf("candles=%d want %d", len(f.Candles), 3*24*60)
	}
	if last := f.Candles[len(f.Candles)-1]; last[4] != 6_500_000 {
		t.Fatalf("last close %d want 6500000", last[4])
	}
	if err := Validate(f, "BTCUSDT", tick); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "h.json")
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seed != f.Seed || got.Symbol != f.Symbol || len(got.Candles) != len(f.Candles) {
		t.Fatalf("header mismatch: %+v", got)
	}
	for i := range f.Candles {
		if got.Candles[i] != f.Candles[i] {
			t.Fatalf("candle %d: %v != %v", i, got.Candles[i], f.Candles[i])
		}
	}
	// Same seed => same dataset (the "same algo" guarantee).
	if again := Generate("BTCUSDT", 42, 3, now, 6_500_000, tick); again.Candles[100] != f.Candles[100] {
		t.Fatal("generation not deterministic")
	}
}

func TestRebaseEndsAtCurrentMinute(t *testing.T) {
	f := Generate("BTCUSDT", 1, 1, 1_000_000_020_000, 6_500_000, tick) // generated in the past
	now := int64(1_790_000_123_456)
	cs := Rebase(f.ToCandles(), now)
	last := cs[len(cs)-1]
	if last.Start+60_000 != now-now%60_000 {
		t.Fatalf("last candle %d does not end at current minute %d", last.Start, now-now%60_000)
	}
	for i := 1; i < len(cs); i++ {
		if cs[i].Start-cs[i-1].Start != 60_000 || cs[i].Start%60_000 != 0 {
			t.Fatalf("rebased candles not contiguous/aligned at %d", i)
		}
	}
	if cs[0].Open != f.Candles[0][1] || last.Close != 6_500_000 {
		t.Fatal("rebase must not change prices")
	}
}

func TestValidateRejectsBadFiles(t *testing.T) {
	good := func() File { return Generate("BTCUSDT", 42, 1, 1_790_000_000_000, 6_500_000, tick) }
	cases := map[string]func(*File){
		"wrong symbol": func(f *File) { f.Symbol = "ETHUSDT" },
		"wrong tick":   func(f *File) { f.TickSize = 1 },
		"wrong scale":  func(f *File) { f.PriceScale = 1 },
		"empty":        func(f *File) { f.Candles = nil },
		"gap":          func(f *File) { f.Candles = append(f.Candles[:10:10], f.Candles[11:]...) },
		"unaligned":    func(f *File) { f.Candles[0][0] += 1 },
		"bad ohlc":     func(f *File) { f.Candles[5][2] = f.Candles[5][3] - 1 }, // high < low
		"neg volume":   func(f *File) { f.Candles[5][5] = -1 },
	}
	for name, mutate := range cases {
		f := good()
		mutate(&f)
		if err := Validate(f, "BTCUSDT", tick); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("want error for missing file")
	}
}
