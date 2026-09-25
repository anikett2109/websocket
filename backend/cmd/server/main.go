// Command server runs the whole backend in one process: synthetic generator,
// market processor, REST API and adaptive WebSocket delivery.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"cryptofeed/internal/api"
	"cryptofeed/internal/config"
	"cryptofeed/internal/generator"
	"cryptofeed/internal/history"
	"cryptofeed/internal/logging"
	"cryptofeed/internal/market"
	"cryptofeed/internal/model"
	"cryptofeed/internal/ws"
)

func main() {
	cfg := config.Load()
	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}
	logOut, closeLogs, err := logging.Setup(logging.Config{
		Output: cfg.LogOutput, Dir: cfg.LogDir, Format: cfg.LogFormat, Level: level,
		MaxBytes: int64(cfg.LogMaxMB) << 20, MaxDays: cfg.LogMaxDays,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "logging:", err)
		os.Exit(1)
	}
	defer closeLogs()
	if cfg.LogOutput != "stdout" {
		// The only terminal output: where to find the logs.
		abs, _ := filepath.Abs(cfg.LogDir)
		fmt.Printf("backend starting on :%s, logs -> %s\n", cfg.Port, abs)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	now := time.Now().UnixMilli()
	past, err := loadHistory(cfg, now)
	if err != nil {
		fatal("load history", "file", cfg.HistoryFile, "err", err)
	}
	// Live trading continues from the last historical close.
	startPrice := past[len(past)-1].Close
	startPrice -= startPrice % cfg.TickSize

	gen := generator.New(generator.Config{
		Seed: cfg.Seed, StartPrice: startPrice, TickSize: cfg.TickSize, DepthSkipPct: cfg.DepthSkipPct,
	})
	mkt := market.New(market.Config{
		Symbol: cfg.Symbol, StartPrice: startPrice, TickSize: cfg.TickSize,
		HistoryCandles: cfg.HistoryCandles, TradeBuffer: cfg.TradeBuffer, StateRing: cfg.StateRing,
	})
	if err := mkt.Seed(past, now, gen.Book()); err != nil {
		fatal("seed market", "err", err)
	}

	events := make(chan generator.Event, 1024)
	go gen.Run(ctx, cfg.TradeInterval, events)
	go mkt.Run(ctx, events)

	wsSrv := ws.NewServer(cfg, mkt)
	go wsSrv.Run(ctx)
	go logRates(ctx, mkt)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.RecoveryWithWriter(logOut)) // panics land in the log file too
	api.New(cfg, mkt, wsSrv).Register(r)
	r.GET(cfg.WSPath, func(c *gin.Context) { wsSrv.Handle(c.Writer, c.Request) })

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: r, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("backend listening", "addr", srv.Addr, "ws", cfg.WSPath, "symbol", cfg.Symbol, "seed", cfg.Seed)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	slog.Info("backend stopped")
}

// fatal logs an error to the configured output (and stderr, so a failed start
// is visible in the terminal) and exits.
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	fmt.Fprintln(os.Stderr, append([]any{"fatal:", msg}, args...)...)
	os.Exit(1)
}

// loadHistory reads the 1m history file into memory and rebases it so it ends
// at the current minute. If the file does not exist, the same dataset is
// generated in memory (same algorithm and seed); a file that exists but is
// invalid is a startup error rather than being silently replaced.
func loadHistory(cfg config.Config, now int64) ([]model.Candle, error) {
	f, err := history.Load(cfg.HistoryFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		slog.Warn("history file not found; generating 3 days in memory (run `go run ./cmd/gendata` to create it)", "file", cfg.HistoryFile)
		f = history.Generate(cfg.Symbol, cfg.Seed, 3, now, cfg.StartPrice, cfg.TickSize)
	case err != nil:
		return nil, err
	}
	if err := history.Validate(f, cfg.Symbol, cfg.TickSize); err != nil {
		return nil, err
	}
	cs := history.Rebase(f.ToCandles(), now)
	slog.Info("history loaded", "file", cfg.HistoryFile, "candles_1m", len(cs), "seed", f.Seed,
		"from", time.UnixMilli(cs[0].Start).Format(time.RFC3339), "last_close", cs[len(cs)-1].Close)
	return cs, nil
}

// logRates prints generator throughput every 30s (not per event).
func logRates(ctx context.Context, m *market.Market) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	var lastT, lastD uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tr, d := m.Counts()
			slog.Info("generator rates", "trades_per_s", float64(tr-lastT)/30, "depth_per_s", float64(d-lastD)/30)
			lastT, lastD = tr, d
		}
	}
}
