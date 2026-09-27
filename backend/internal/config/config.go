// Package config loads service configuration from environment variables.
// Every value has a sensible default so `go run ./cmd/server` works with no setup.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// TierRates are the delivery cadences for one tier. They are delivery rates,
// not market-generation rates: the market engine processes every event at all tiers.
type TierRates struct {
	Depth time.Duration
	Chart time.Duration
	Trade time.Duration
}

type Config struct {
	Port           string
	WSPath         string
	Symbol         string
	AllowedOrigins []string
	Debug          bool

	LogOutput  string // file | stdout | both
	LogDir     string
	LogFormat  string // json | text
	LogMaxMB   int
	LogMaxDays int

	BasePrice      int64  // fixed-point, PriceScale: centre of the deterministic price path
	TickSize       int64  // fixed-point, PriceScale
	HistoryFile    string // 1m candle file loaded into the cache at startup
	HistoryCandles int    // cache capacity per interval
	TradeBuffer    int
	StateRing      int

	Full     TierRates
	Degraded TierRates
	Minimal  TierRates

	WarmupSamples      int
	ReportDegradeAfter time.Duration
	ReportMinimalAfter time.Duration
}

func Load() Config {
	return Config{
		Port:           str("SERVER_PORT", str("PORT", "8080")),
		WSPath:         str("WS_PATH", "/ws"),
		Symbol:         str("SYMBOL", "BTCUSDT"),
		AllowedOrigins: list("ALLOWED_ORIGINS", "*"),
		Debug:          str("LOG_LEVEL", "info") == "debug",

		LogOutput:  str("LOG_OUTPUT", "file"),
		LogDir:     str("LOG_DIR", "logs"),
		LogFormat:  str("LOG_FORMAT", "json"),
		LogMaxMB:   num("LOG_MAX_MB", 50),
		LogMaxDays: num("LOG_MAX_DAYS", 7),

		BasePrice:      int64(num("BASE_PRICE_CENTS", 6500000)), // 65000.00
		TickSize:       int64(num("TICK_SIZE_CENTS", 50)),       // 0.50
		HistoryFile:    str("HISTORY_FILE", "data/history_1m.json"),
		HistoryCandles: num("HISTORY_CANDLES", 3*24*60), // 3 days of 1m
		TradeBuffer:    num("TRADE_BUFFER", 20000),
		StateRing:      num("STATE_RING", 4096),

		// Delivery intervals are multiples of the 50 ms market tick (see README
		// "Numbers and their derivation"): chart 100 / 300 / 1000 ms is a
		// geometric series (×√10), depth = chart/2 (≥ 1 tick), trades from the
		// latest-10 window (10 trades at 20/s span 500 ms).
		Full:     TierRates{ms("FULL_DEPTH_MS", 50), ms("FULL_CHART_MS", 100), ms("FULL_TRADE_MS", 250)},
		Degraded: TierRates{ms("DEGRADED_DEPTH_MS", 150), ms("DEGRADED_CHART_MS", 300), ms("DEGRADED_TRADE_MS", 500)},
		Minimal:  TierRates{ms("MINIMAL_DEPTH_MS", 500), ms("MINIMAL_CHART_MS", 1000), ms("MINIMAL_TRADE_MS", 2000)},

		WarmupSamples:      num("WARMUP_SAMPLES", 3),
		ReportDegradeAfter: ms("REPORT_DEGRADE_MS", 3000),
		ReportMinimalAfter: ms("REPORT_MINIMAL_MS", 6000),
	}
}

func str(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func num(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return def
}

func ms(key string, def int) time.Duration {
	return time.Duration(num(key, def)) * time.Millisecond
}

func list(key, def string) []string {
	var out []string
	for _, p := range strings.Split(str(key, def), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
