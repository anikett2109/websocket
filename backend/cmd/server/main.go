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

	// Everything is a function of the market tick; live processing starts at n0.
	mdl := generator.Model{Base: cfg.BasePrice, Tick: cfg.TickSize}
	n0 := generator.TickAt(time.Now().UnixMilli())
	past, err := loadHistory(cfg, mdl, n0)
	if err != nil {
		fatal("load history", "file", cfg.HistoryFile, "err", err)
	}
	mkt := market.New(market.Config{
		Symbol: cfg.Symbol, BasePrice: cfg.BasePrice, TickSize: cfg.TickSize,
		HistoryCandles: cfg.HistoryCandles, TradeBuffer: cfg.TradeBuffer, StateRing: cfg.StateRing,
	})
	// Recent trades (the ring's worth, oldest first) so /api/trades and the
	// latest-10 panel are populated immediately, identically after a restart.
	var recent []model.Trade
	for n := n0 - min(n0, uint32(cfg.TradeBuffer)); n < n0; n++ {
		recent = append(recent, mdl.Trades(n)...)
	}
	if err := mkt.Seed(past, generator.TickTime(n0), generator.LastTradeTick(n0-1), recent, mdl.Book(n0-1)); err != nil {
		fatal("seed market", "err", err)
	}

	events := make(chan generator.Event, 1024)
	go generator.New(mdl, n0).Run(ctx, events)
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
		slog.Info("backend listening", "addr", srv.Addr, "ws", cfg.WSPath, "symbol", cfg.Symbol, "tick", n0)
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

// loadHistory reads the 3-day 1m history file and completes it up to tick n0
// (including the partial current minute) from the same market function, so
// file, gap and live data form one continuous series. A missing file means
// everything is computed; a file that exists but is invalid is a startup error.
func loadHistory(cfg config.Config, mdl generator.Model, n0 uint32) ([]model.Candle, error) {
	const days = 3
	var fromFile []model.Candle
	f, err := history.Load(cfg.HistoryFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		slog.Warn("history file not found; computing 3 days from the market function (run `go run ./cmd/gendata` to create it)", "file", cfg.HistoryFile)
	case err != nil:
		return nil, err
	default:
		if err := history.Validate(f, cfg.Symbol, mdl); err != nil {
			return nil, err
		}
		fromFile = f.ToCandles()
	}
	cs := history.Window(fromFile, mdl, days, n0)
	if len(cs) == 0 {
		return nil, errors.New("no history")
	}
	slog.Info("history loaded", "file", cfg.HistoryFile, "file_candles", len(fromFile), "candles_1m", len(cs),
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
