package history

import (
	"errors"
	"path/filepath"
	"testing"

	"cryptofeed/internal/generator"
)

var mdl = generator.Model{Base: 6_500_000, Tick: 50}

// A time well after the generator epoch, mid-minute.
var now = generator.Epoch + 250*24*3600*1000 + 37_250

func TestGenerateSaveLoadRoundTrip(t *testing.T) {
	f := Generate("BTCUSDT", mdl, 1, now)
	if len(f.Candles) != 24*60 {
		t.Fatalf("candles=%d want %d", len(f.Candles), 24*60)
	}
	if err := Validate(f, "BTCUSDT", mdl); err != nil {
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
	if got.Generator != Generator || got.BasePrice != f.BasePrice || len(got.Candles) != len(f.Candles) {
		t.Fatalf("header mismatch: %+v", got)
	}
	for i := range f.Candles {
		if got.Candles[i] != f.Candles[i] {
			t.Fatalf("candle %d: %v != %v", i, got.Candles[i], f.Candles[i])
		}
	}
}

// The file and the live function are one series: loading an older file and
// filling up to now gives exactly what computing everything would give.
func TestWindowEqualsFunction(t *testing.T) {
	nowTick := generator.TickAt(now)
	older := Generate("BTCUSDT", mdl, 1, now-3*3600*1000) // generated 3 h ago
	got := Window(older.ToCandles(), mdl, 1, nowTick)
	want := Window(nil, mdl, 1, nowTick)
	if len(got) != len(want) {
		t.Fatalf("len %d != %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Start != w.Start || g.Open != w.Open || g.High != w.High || g.Low != w.Low || g.Close != w.Close || g.Volume != w.Volume || g.CloseSeq != w.CloseSeq {
			t.Fatalf("candle %d differs: file %+v vs function %+v", i, g, w)
		}
	}
	last := got[len(got)-1]
	if last.Start != now-now%minute {
		t.Fatalf("last candle %d should be the partial current minute %d", last.Start, now-now%minute)
	}
}

func TestValidateRejectsBadFiles(t *testing.T) {
	good := func() File { return Generate("BTCUSDT", mdl, 1, now) }
	cases := map[string]func(*File){
		"wrong symbol": func(f *File) { f.Symbol = "ETHUSDT" },
		"wrong tick":   func(f *File) { f.TickSize = 1 },
		"wrong base":   func(f *File) { f.BasePrice = 1 },
		"wrong model":  func(f *File) { f.Generator = "random" },
		"wrong scale":  func(f *File) { f.PriceScale = 1 },
		"empty":        func(f *File) { f.Candles = nil },
		"gap":          func(f *File) { f.Candles = append(f.Candles[:10:10], f.Candles[11:]...) },
		"unaligned":    func(f *File) { f.Candles[0][0] += 1 },
		"bad ohlc":     func(f *File) { f.Candles[5][2] = f.Candles[5][3] - 1 },
		"neg volume":   func(f *File) { f.Candles[5][5] = -1 },
	}
	for name, mutate := range cases {
		f := good()
		mutate(&f)
		if err := Validate(f, "BTCUSDT", mdl); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("want error for missing file")
	}
}
