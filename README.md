# BTCUSDT Adaptive Feed

A simulated crypto market (Go backend) and a Next.js trading screen. Live chart delivery adapts to each client's connection, and the market data stays correct at every tier.

**Live:** https://websocket-psi-ten.vercel.app · backend https://cryptofeed-backend.onrender.com (free instance; the first request after idle can take about a minute)

```
backend/    Go 1.25 · Gin (REST) · gorilla/websocket · in-memory state, one process
frontend/   Next.js 16 (App Router) · React 19 · TypeScript · Zustand · lightweight-charts
```

## 1. Frontend and backend architecture

```
Generator (one event per 50 ms tick) ─► Market processor (single writer)
                                          ├─ candles 1m / 5m ─┐ rings of recent states,
                                          ├─ order book ──────┘ keyed by tick
                                          └─ trade ring (20k)
                                                 │ publishes tick n
              REST /api/* ◄──────────────────────┼──────────► 3 tier hubs (FULL / DEGRADED / MINIMAL)
              snapshots, history, trades                       each stream fires when n % k == 0
                                                               one frame per client per tick
```

- **The market processor decides *what* happened; the hubs decide *when* each client hears it.** Every trade is applied regardless of tier, so candles are identical at every tier.
- **One WebSocket per browser.** The server puts each connection in one of three hubs. Packets due on the same tick go out as one binary frame.
- **Backpressure:** sends never block. If a client's buffer is full, that flush is skipped and its last-sent sequence doesn't move, so the next flush sends one larger delta.

**Frontend.** `FeedClient` (`src/lib/feed.ts`) owns the socket, REST calls, sync state machines, latency probes and reconnects. `StreamSync` applies snapshots and deltas, the Zustand store holds UI state, and components render it. App Router: pages are server components hosting client islands; the root layout keeps one socket across routes.

## 2. State-management choices

- **Logic outside React.** `FeedClient` is a plain class, so it can be tested headless (the e2e test runs it in Node).
- **Zustand as a passive store.** Changes are batched and committed at most once per animation frame, so 20+ updates/s never cause more than one render per frame.
- **Narrow selectors.** A depth update doesn't re-render the chart. The chart subscribes directly and calls `series.update()`, bypassing React rendering.
- **Why Zustand:** no reducers are needed (logic lives in `FeedClient`), selectors limit re-renders, and it can be updated from outside React.

## 3. Generated data and REST/WebSocket protocols

### Generated data

Everything is a **pure function of the market tick** n = ⌊(t − 2026-01-01Z) / 50 ms⌋. There's no randomness, so the feed is repeatable and a restart resumes the same prices, ids and sequences.

| Regime (60 s cycle) | Duration | Trades | Book changes |
|---|---|---|---|
| Normal | 30 s | 20/s | every tick |
| Burst | 10 s | 60/s, ±$60 move | every tick |
| Quiet | 20 s | 1/s | every 5th tick |

- **Price:** 65,000 plus four sine waves (6 h, 97 min, 4 min, 37 s) plus the burst move, on a $0.50 grid. The book is 10 levels per side around it with a one-tick spread. Even trade ids buy at the ask, odd ids sell at the bid.
- **History:** `backend/data/history_1m.json` holds 3 days of 1m candles from the same function (`go run ./cmd/gendata`). At startup any missing minutes up to now are computed; 5m candles are aggregated from 1m.
- **Precision:** prices ×100 and quantities ×1e6 are integers end to end.

### REST

| Endpoint | Returns |
|---|---|
| `GET /api/candles?interval=1m\|5m&limit=` | `{seq, candles[], active}` (`seq` is the sync base) |
| `GET /api/orderbook/snapshot` | `{seq, bids[10], asks[10]}` |
| `GET /api/trades?from=&to=&limit=` | trades newest first |
| `GET /api/ticker` · `/api/meta` · `/api/health` · `/api/ws/status` | 24h stats · scales · health · per-client tiers |
| `POST /api/debug/clients/:id/tier` | debug override |

### WebSocket (binary, little-endian)

Header (15 B): `type u8 | length u16 | seq u32 | timestamp i64`.

| Packet | Size | Contents |
|---|---|---|
| `CHART_DELTA` | 63 B | seq = tick, ts = candle start, `baseSeq`, ΔOHLCV |
| `DEPTH_DELTA` | 115 B | seq = tick, `baseSeq`, best bid/ask, 10 + 10 quantity deltas |
| `TRADE_UPDATE` | 143 B | latest 10 trades (ids = seq … seq−9, side = sign of qty) |
| `PING` / `PONG` | 15 B | probe seq and the browser's timestamp, echoed |

JSON control messages:
- **Server → client:** `HELLO`, `TIER`, `SUBSCRIBED`, `SYNCED`, `SYNC_FAILED`, `RESYNC`, `ERROR`, `DEBUG_ACK`.
- **Client → server:** `SUBSCRIBE`, `SYNC`, `NET_REPORT`, `SET_TIER_OVERRIDE`, `DEBUG`.

## 4. Chart and order-book synchronization

**One clock for every stream:** a sequence is the tick at which that stream last changed. A tier only chooses which ticks it samples, e.g. FULL chart `100→102→104`, DEGRADED `100→106→112`.

- **Rule:** apply a delta only if `baseSeq == local seq`. A higher base means a gap, so re-snapshot. A duplicate is ignored.
- **Start and recovery:** fetch the REST snapshot (seq S) while buffering live deltas. If the buffer chains from S, apply it; otherwise send `SYNC{S}` and the server continues from S.
- **Candles:** the packet timestamp identifies the candle. A newer start closes the current candle and opens a new one.
- **Interval switch:** `SUBSCRIBE` → REST → `SYNC`; late responses for the old interval are discarded.

Depth and chart deltas are eventually consistent: any missed state is folded into the next delta. Trades are different, see §11.

## 5. Latency and jitter measurement

1. The browser sends `PING` 1/s (3 quick probes at start); the server echoes it as `PONG`. RTT = now − echoed timestamp, using the browser's own clock.
2. Over the last 5 RTTs: **latency = median**, **jitter = MAD** (median of |RTT − latency|).
3. The browser sends `NET_REPORT {latencyMs, jitterMs}`. The server scores **L = latency + 4·jitter**.

The median ignores one or two spikes; a real change shows up within 3 probes. Example: [89, 90, 88, 118, 91] → latency 90, jitter 1, L = 94.

## 6. Tier thresholds, hysteresis and missing-report fallback

| Tier | L | Depth / chart / trades |
|---|---|---|
| FULL | < 200 ms | 50 / 100 / 250 ms |
| DEGRADED | 200–600 ms | 150 / 300 / 500 ms |
| MINIMAL | ≥ 600 ms | 500 / 1000 / 2000 ms |

- **Threshold = 2 × the tier's chart interval**, so at most one update is in flight.
- **Hysteresis:** demote after 3 reports past a threshold; promote after 5 reports below 80% of it (160 / 480 ms).
- **Missing reports:** 3 s of silence → at most DEGRADED, 6 s → MINIMAL. When reports resume, the server re-classifies after 3 reports.
- **Server location** sets the ceiling. From India:

| Server | Distance | RTT | L | Tier |
|---|---|---|---|---|
| Singapore (deployed) | 3,160 km | 77–125 ms (measured) | ≈ 108 | FULL |
| Frankfurt | ~6,300 km | ≈ 130–185 ms | ≈ 170 | FULL/DEGRADED border |
| US West | ~13,000 km | ≈ 230–295 ms | ≈ 280 | DEGRADED |
| GEO satellite | — | ≈ 600 ms+ | ≥ 600 | MINIMAL |

## 7. Reconnect, browser lifecycle and stale-state behavior

- **Reconnect:** exponential backoff (0.5 s × 2ⁿ, max 10 s, with jitter), then re-subscribe and re-snapshot. Immediate on `online`, tab visible, or restore from the back/forward cache.
- **Stale state:** while disconnected, panels keep their last values, greyed out and labelled STALE, with a retry countdown.
- **Hidden tab:** the socket stays open and syncing continues, but rendering and latency reports pause (so the tab drops to MINIMAL). On return it re-classifies within about a second.
- **Malformed input** is dropped and logged on both sides; the connection stays up.
- **Cleanup:** `dispose()` closes the socket and clears timers, listeners and requests.

## 8. Debug controls

| Control | Effect |
|---|---|
| Force tier (AUTO / FULL / DEGRADED / MINIMAL) | Override the tier; AUTO returns to automatic |
| Simulated latency +300 ms / +700 ms | The browser adds it to every RTT → automatic DEGRADED / MINIMAL |
| Spike | +900 ms on one sample; the tier should not change |
| Drop depth / chart packet | Server skips one packet; the client detects the gap and recovers |
| Kill socket / Go offline | Test reconnect and the stale display |

## 9. Packages used

- **Backend:** `gin-gonic/gin`, `gorilla/websocket`; everything else is the Go standard library.
- **Frontend:** `next`, `react`, `zustand`, `lightweight-charts` (rendering only), `tailwindcss`, `vitest`.

## 10. Local development and deployment

```bash
cd backend  && go run ./cmd/server          # http://localhost:8080, ws://localhost:8080/ws
cd frontend && npm install && npm run dev   # http://localhost:3000

cd backend  && go test ./...
cd frontend && npm test                                       # unit tests
```

- **Frontend config:** `NEXT_PUBLIC_API_URL` (the WebSocket URL is derived from it).
- **Backend config:** environment variables with defaults in `backend/internal/config/config.go` (port, allowed origins, tier rates, logging).
- **Deployment:** the frontend runs on **Vercel** (static build, root `frontend`). The backend runs on **Render** from `render.yaml` (Docker, free plan, Singapore, one instance because state is in memory; it sleeps after 15 min idle). CI (`.github/workflows/ci.yml`) runs the tests and builds.
- **Logs:** locally, JSON lines in `backend/logs/backend-YYYY-MM-DD.log` (daily and 50 MB rotation, 7-day retention). On Render, they go to stdout and the Render Logs tab.

## 11. Known limitations

- **Trades are a window, not a log.** Depth and chart are eventually consistent: a slower tier still reaches the exact same state, because missed states are merged into the next delta. Trades aren't merged: each tier just receives the latest 10 at its rate. At MINIMAL, trades between flushes are visible only on the REST history page.
- **Synthetic, periodic market.** A sum of sines with a 60 s regime cycle: repeatable, but visibly regular.
- **Single in-memory instance.** Only one backend process can run, and trade history is limited to recent trades.
- **Backend on Renderer free plan**- The backend is hosted on Render's free tier, which causes it to spin down after 15 minutes of inactivity. As a result, initial requests experience a noticeable delay while the server warms up.