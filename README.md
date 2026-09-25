# BTCUSDT Adaptive Feed

A simulated crypto market (Go backend) and a Next.js trading screen. Live chart delivery adapts to each client's connection quality, and the market data stays correct at every tier.

```
backend/    Go 1.25 · Gin (REST) · gorilla/websocket · in-memory state, one process
frontend/   Next.js 16 (App Router) · React 19 · TypeScript · Zustand · lightweight-charts
```

## Quick start

```bash
# backend (one command) — http://localhost:8080, WebSocket ws://localhost:8080/ws
cd backend && go run ./cmd/server

# optional: regenerate the 3-day history file the backend loads at startup
cd backend && go run ./cmd/gendata          # -days 3 -seed 42 -close 6500000

# frontend — http://localhost:3000
cd frontend && npm install && npm run dev
```

The browser reads `NEXT_PUBLIC_API_URL` (default `http://localhost:8080`) and derives the WebSocket URL from it (`http→ws` plus `/ws`). Set `NEXT_PUBLIC_WS_URL` to override that. In a deployed environment, point both at the backend's public HTTPS/WSS origin (see [Deployment](#deployment)).

Tests:

```bash
cd backend  && go test ./...            # CI also runs: go test -race ./...
cd frontend && npm test                 # unit tests
E2E_API_URL=http://localhost:8080 npm test   # + live end-to-end test (backend must be running)
```

---

## 1. Architecture

```
 Generator ──(chan Event, every 50 ms)──► Market processor  (single writer goroutine)
   seeded PRNG: trade + book                  ├─ candle.Series 1m ┐ ring of canonical states
                                              ├─ candle.Series 5m ┘ keyed by chartSeq
                                              ├─ trades.Store (ring, 20k)
                                              └─ orderbook.Engine ── ring of books keyed by depthSeq
                                                         │  RWMutex reads
                    ┌────────────────────────────────────┼─────────────────────────────┐
                    ▼                                    ▼                             ▼
          REST (Gin) /api/*                  ws.Server = client manager         3 tier hubs
          history · snapshots · trades       conn lifecycle · tier state        FULL / DEGRADED / MINIMAL
                                             machine · overrides ──moves──►     own tickers per stream
                                                                                one writer goroutine per conn
```

**What happened vs. when it's delivered.** The market processor applies every generated event, whatever tier the clients are on. The WebSocket layer never computes candles and never owns the book. It reads canonical state and decides when each client receives it. A slower tier only changes how often updates are delivered. It never changes the final OHLCV values.

**Tier hubs.** There is one WebSocket per browser. The client manager puts each connection into one of three hubs, and each hub has its own depth, chart and trade tickers. Clients flushed on the same tick converge on the same base sequence, so each packet is encoded once per `(stream, base)` and shared across those clients. A client that just moved to a new hub gets one catch-up delta computed from its own last sequence. After that it is in step with the rest of the hub.

**Backpressure.** Sends never block. If a client's buffer is full, that flush is skipped and the client's last-sent sequence doesn't advance, so the next flush sends one larger coalesced delta. Data is never silently lost.

### Backend packages

| Package | Responsibility |
|---|---|
| `generator` | Deterministic market: a random-walk mid price, trades at the touch, a 10×10 book on a contiguous tick grid, and seeded 1m history |
| `market` | Canonical state; single writer; consistent read views for REST and the hubs |
| `candle` | Per-interval OHLCV, a ring of states by `chartSeq`, and coalesced `Transitions(base)` |
| `orderbook` | Authoritative book, `depthSeq`, a state ring, and `Diff`/`ApplyDelta` |
| `trades` | Bounded ring buffer: the latest 10 plus time-range queries |
| `history` | History file format: generate, save, load, validate, rebase (`cmd/gendata` writes it) |
| `logging` | slog setup plus a dependency-free rotating file writer (daily and by size, with retention) |
| `protocol` | Binary encode/decode (`encoding/binary`, little-endian) |
| `client` | The tier state machine (hysteresis and missing reports); pure and unit-tested |
| `ws` | Client manager, tier hubs, per-connection reader/writer, controls |
| `api` | REST handlers, CORS, the debug endpoint |

### Frontend

```
src/lib/protocol.ts        binary decoder / PING encoder
src/lib/sync/streamSync.ts generic "snapshot + ordered deltas" state machine
src/lib/sync/book.ts       depth delta application
src/lib/sync/chart.ts      chart delta application, candle rollover, dedupe
src/lib/latency.ts         SRTT/RTTVAR estimator, rate counters
src/lib/feed.ts            FeedClient: socket, REST, reconnect, lifecycle, batching
src/store/market.ts        Zustand store (passive sink)
src/components/*           UI (chart, book, trades, tier, debug, log)
src/app/page.tsx           trading screen · src/app/trades/page.tsx  trade history
```

**App Router.** Pages are server components that lay out client "islands". The root layout mounts one `FeedProvider`, so the socket survives navigating to `/trades` and back. The App Router gives us layouts and metadata for free. Everything live is client-side by nature.

## 2. State management

- **Networking and sync live outside React.** `FeedClient` is a plain TypeScript class. It owns the socket, the REST calls, both sync state machines, the latency probes, reconnection and the browser-lifecycle handling. It can be unit-tested and run headless in Node, which the end-to-end test does.
- **The store is a passive Zustand sink.** `FeedClient` merges changes into a pending patch and commits it **at most once per animation frame**. At 20 depth updates/s plus chart and trade updates, React re-renders at most at frame rate, and hidden tabs don't render at all.
- **Narrow selectors.** Components subscribe to narrow slices, so a depth update doesn't re-render the chart. The chart goes further: it subscribes imperatively (`useMarket.subscribe`) and calls `series.update()`, so candles never pass through React rendering.

Zustand was chosen over Redux or Context because the store needs no reducers or middleware (all logic is in `FeedClient`), it offers selector-based subscriptions, and it can be subscribed to from outside React.

## 3. Generated data

- **Trades:** one per 50 ms (about 20/s). Each trade has a monotonically increasing `id` (`uint32`; also the chart sequence), `ts` (unix ms), `price`, `qty`, and `side`. Trades execute at the best bid or ask (occasionally one level through) and consume liquidity.
- **Book:** 10 bids and 10 asks on a contiguous 0.50 tick grid around a random-walking mid price. The spread is 1 tick, occasionally 2. It is updated on about 85% of ticks, giving roughly 50 ms updates with occasional 100 ms gaps. The book always keeps `bestBid < bestAsk` and positive quantities, and the engine validates every update.
- **History file:** `backend/data/history_1m.json` holds **3 days of 1m candles** (4,320 rows, about 250 KB), created by `go run ./cmd/gendata` with the same deterministic `generator.History` algorithm and seed.
  - **Format:** a JSON header (symbol, scales, tick, seed, generatedAt), then one `[t, o, h, l, c, v]` row per line, as fixed-point integers.
  - **At startup** the server loads and validates the file: symbol, scales and tick must match; candles must be contiguous and minute-aligned; OHLCV must be consistent. It then *rebases* the timestamps by whole minutes so the last candle ends at the current minute, and loads the candles into the candle cache.
  - **Other intervals:** 5m candles are aggregated from the same 1m rows, so both intervals agree. The minutes already inside the current 5m window seed the active 5m candle.
  - **Live continuity:** live trading continues from the file's last close, so there is no price jump between history and live data.
  - **Missing file:** the same 3 days are generated in memory, with a warning. A file that exists but is invalid is a startup error, never silently replaced.
- **Repeatability:** `RANDOM_SEED` (default 42) fixes the entire event sequence: every trade's price and quantity and every book shape. Only wall-clock timestamps differ between runs. `generator_test.go` checks this.
- **Precision:** prices are integers ×100 (0.01 USDT) and quantities are integers ×1e6 (0.000001 BTC), end to end. The browser keeps them as integers, which are safe up to 2⁵³, and formats them without floating-point math. Only the chart library receives floats, and only for drawing.

## 4. REST API

All values are fixed-point integers. `GET /api/meta` returns the scales.

| Endpoint | Returns |
|---|---|
| `GET /api/health` | status, uptime, events processed, client count |
| `GET /api/meta` | scales, tick size, intervals, packet sizes |
| `GET /api/ticker` | LTP and 24h open, high, low, volume (from 5m candles) |
| `GET /api/candles?symbol=BTCUSDT&interval=1m\|5m&limit=1..5000` | `{seq, ltp, candles[], active}`. `seq` is the chart base for SYNC. |
| `GET /api/orderbook/snapshot?symbol=BTCUSDT` | `{seq, ltp, ltq, tickSize, bids[10], asks[10]}` |
| `GET /api/trades?symbol=BTCUSDT&from=ms&to=ms&limit=1..5000` | newest first, from the ring buffer, plus `retainedFrom` |
| `GET /api/ws/status` | per-connection tier, SRTT, RTTVAR, E, override, sequences, packets sent |
| `POST /api/debug/clients/:id/tier` `{"tier":"AUTO\|FULL\|DEGRADED\|MINIMAL"}` | debug override |

Invalid intervals, limits, ranges and symbols return `400`/`404` with `{"error": "..."}`. Empty history returns `candles: []` and `active: null`.

## 5. WebSocket protocol

Market data is binary, little-endian, with a 15-byte header on every packet.

```
type u8 | length u16 (whole packet) | seq u32 | timestamp i64
```

| Type | Size | Header `seq` / `ts` | Payload |
|---|---|---|---|
| 1 `CHART_DELTA` | **63 B** | chartSeq / candle start | `baseSeq` (i64 slot), Δopen, Δhigh, Δlow, Δclose, Δvolume (i64) |
| 2 `DEPTH_DELTA` | **115 B** | depthSeq / event time | `baseSeq` u32, `bestBid` i64, `bestAsk` i64, 10 bid Δqty i32, 10 ask Δqty i32 |
| 3 `TRADE_UPDATE` | **143 B** | newest trade id / newest trade ts | LTP i64, then 10 × (Δprice i32, ±qty i32, Δtime i32) |
| 4 `PING` | **15 B** | probe seq / client clock (µs) | none (client → server) |
| 5 `PONG` | **15 B** | the same values echoed | none (server → client) |

Design notes:

- **`baseSeq` in CHART_DELTA** occupies the slot originally planned for `ltpDelta`. That field is redundant because the active candle's close always equals the LTP, so reusing the slot keeps the packet at 63 B.
- **`bestBid`/`bestAsk` in DEPTH_DELTA** make the packet self-describing when the book shifts price. Level `i` sits at `bestBid − i·tick` / `bestAsk + i·tick`. Each quantity delta is relative to the client's quantity *at that same price* (0 if the client had no level there), so a shifted book re-indexes naturally. A level whose quantity reaches zero is removed.
- **TRADE_UPDATE** carries the latest 10 trades, newest first. The trade ids are consecutive (`seq`, `seq−1`, … `seq−9`), so every trade keeps its unambiguous ordering id without extra bytes. The sign of the quantity encodes the aggressor side (+ buy, − sell). Price and time are deltas from the packet's LTP and timestamp.

JSON text frames carry control messages:

| Direction | Message |
|---|---|
| server → client | `HELLO {connId, scales, tickSize, tiers}`, `TIER {tier, autoTier, override, reason, rates, effectiveLatencyMs, …}`, `SUBSCRIBED`, `SYNCED {stream, seq}`, `SYNC_FAILED`, `RESYNC {stream}`, `ERROR`, `DEBUG_ACK` |
| client → server | `SUBSCRIBE {symbol, interval}`, `SYNC {stream, seq, interval?}`, `NET_REPORT {rttMs, srttMs, rttvarMs, samples}`, `SET_TIER_OVERRIDE {tier}`, `DEBUG {action: DROP_DEPTH\|DROP_CHART\|SIM_LATENCY, ms}` |

## 6. Sequences and synchronisation (chart and order book)

There is **one canonical sequence per stream**, never one per tier:

- `chartSeq` is the trade id. Every trade is one chart state transition, for both 1m and 5m.
- `depthSeq` increments with every canonical book update.
- Trades use their own trade ids.

A tier only decides *which* canonical states are delivered and when. A FULL client might receive `0→1, 1→2, 2→3`, while a MINIMAL client receives `0→10` as one net delta.

**Invariant:** a delta is applied only if `packet.baseSeq == local seq`.

The deltas are additive and may be coalesced, so a packet can never be partially applied. Sync therefore works like this (`streamSync.ts`, used for both streams):

1. **Snapshot.** Request the REST snapshot and buffer live deltas while it is in flight.
2. **Snapshot arrives at seq S.** Drop buffered deltas with `seq ≤ S`, since the snapshot already includes them.
   - If the remaining deltas chain exactly from S, apply them and go **live** (the fast path).
   - Otherwise send `SYNC{S}`. The server looks up state S in its ring buffer and rebases this connection onto it. The server's `SYNCED{S}` ack is enqueued under the connection lock, and the socket is ordered, so every delta after the ack is relative to S. Deltas that arrive between `SYNC` and `SYNCED` belong to the old baseline and are ignored. This converges in one round trip at any tier.
3. **Live.**
   - `base == seq`: apply.
   - `seq ≤ local`: a duplicate, ignore it.
   - Anything else is a **gap**: go back to step 1.
   A delta that would produce a negative quantity or an older candle also triggers a re-snapshot.
4. **Late responses.** Every snapshot request carries a token. A response for an older token, or for an interval the user has since switched away from, is discarded. Superseded fetches are aborted.

**Chart specifics.**
- `SUBSCRIBE` clears the server's chart baseline, so no chart packets flow until the client SYNCs. The client drops chart packets until it receives the `SUBSCRIBED` ack, because anything earlier belongs to the old interval.
- The header timestamp identifies the candle:
  - Same start as the local active candle: add the deltas.
  - Newer start: finalise the local candle and open a new one whose values are the deltas *from zero*.
  - Older start: resync.
- If a slow tier's flush crosses a candle boundary, the server sends one transition that finalises the old candle, then one for the new candle.
- History is sorted and de-duplicated by timestamp.

**Server retention.** Ring buffers hold 4096 states per stream (about 3.4 min at 20/s). If a client's base has aged out, the server sends `RESYNC` or `SYNC_FAILED` and the client re-snapshots.

## 7. Latency and jitter measurement

The client measures on its own clock, reports the results, and the server decides the tier.

- Every **1 s** the app sends a binary `PING(probeSeq, performance.now() µs)`. The server echoes it immediately as a `PONG`.
- `RTT = now − echoedTimestamp`. Both timestamps come from the browser's monotonic clock, so no clock synchronisation is needed. Invalid or future probes are ignored.
- Smoothing follows RFC 6298:
  - First sample: `SRTT = RTT`, `RTTVAR = RTT/2`.
  - After that: `RTTVAR = ¾·RTTVAR + ¼·|SRTT − RTT|` and `SRTT = ⅞·SRTT + ⅛·RTT`.
- **Jitter is RTTVAR**, the smoothed mean deviation of RTT.
- After each sample the app sends `NET_REPORT {rttMs, srttMs, rttvarMs}`.
- The server scores the connection as **`E = SRTT + 4·RTTVAR`**. This is the RFC 6298 retransmission-timeout form: a pessimistic latency bound that penalises jitter as well as delay.
- **Hidden tabs.** Browsers throttle timers in hidden tabs, which would measure the throttling rather than the network. While the tab is hidden, samples are skipped and not reported, and the estimator resets when the tab becomes visible again.

## 8. Tiers, thresholds and hysteresis

| Tier | E range | Depth | Chart | Trades |
|---|---|---|---|---|
| **FULL** | E < 100 ms | 50 ms (20/s) | 100 ms (10/s) | 250 ms (4/s) |
| **DEGRADED** | 100 ≤ E < 250 ms | 100 ms (10/s) | 250 ms (4/s) | 500 ms (2/s) |
| **MINIMAL** | E ≥ 250 ms | 250 ms (4/s) | 1000 ms (1/s) | 2000 ms (0.5/s) |

Why these values:

- **FULL matches generation.** Depth is flushed every 50 ms, the same rate the book is generated.
- **100 ms boundary.** Below it, frequent updates still arrive before the next one is due.
- **250 ms boundary.** At or above it, a 10/s chart feed would queue on a link that can't round-trip in time.
- **Rate ordering.** Each tier keeps depth > chart > trades. The book is what traders act on, the chart moves perceptibly at 1–10 Hz, and trades are a list of the latest 10.
- **Configurable.** Every rate can be changed through environment variables (see [Configuration](#configuration)).

**Hysteresis** (`internal/client/tier.go`). Transitions move one step at a time:

| Transition | Condition |
|---|---|
| FULL → DEGRADED | E ≥ 100 ms for **3** consecutive reports |
| DEGRADED → MINIMAL | E ≥ 250 ms for **3** consecutive reports |
| MINIMAL → DEGRADED | E < 200 ms for **5** consecutive reports |
| DEGRADED → FULL | E < 80 ms for **5** consecutive reports |

- The promotion boundaries (80 and 200) sit inside the demotion boundaries (100 and 250), so a score hovering at a boundary can't cause flapping.
- Promotion is slower (5 reports) than demotion (3), because recovering too early is worse than degrading slightly late.
- A counter resets as soon as its condition breaks.
- **Warmup.** A connection starts in DEGRADED, the safe middle, and is classified directly after 5 reports.

**Missing reports.** Silence is treated as a bad connection:
- More than 3 s without a report: capped at DEGRADED.
- More than 6 s without a report: MINIMAL.
- When reports resume, the connection is promoted through normal hysteresis.

**Connection drops.** The server's reader exits and the connection is removed from its hub, its timers and goroutines stop, and its state is released. A WebSocket control ping every 15 s, with a 45 s read deadline, detects dead TCP connections.

**Tier changes never create a new sequence space.** The client's `lastSeq` values carry over, and the first packet in the new hub is a catch-up delta from that sequence.

**Candle correctness across tiers.** `candle_test.go` applies coalesced transitions at cadences of 1, 2, 5, 20, 400 and 1500 trades per flush and checks that every closed and active candle equals the canonical one. The end-to-end test checks the same thing on the live system at a forced MINIMAL tier.

**Display.** The UI shows the tier (automatic or forced), the reason for the last change, the target rate per stream, the measured received/s, and RTT, SRTT, RTTVAR and E.

## 9. Reconnect, browser lifecycle and stale state

- **Reconnect** uses exponential backoff with jitter: 0.5 s × 2ⁿ, capped at 10 s, ±30%. After reconnecting, the client receives `HELLO`, then re-subscribes and re-snapshots both streams. Reconnection is immediate on the `online` event, when the tab becomes visible, and when the page is restored from the back/forward cache (`pageshow`). The socket is closed on `pagehide`.
- **Stale state.** When disconnected, the book, chart and trades keep their cached values but are greyed out and labelled STALE. The connection badge shows how long the data has been stale and a countdown to the next retry. While resynchronising, a panel shows SYNCING.
- **Hidden tab.** The socket stays open and deltas are still applied (they're cheap), so the state is current when the tab returns. Rendering pauses because commits are frame-driven, and latency reports pause, so the server moves the tab to MINIMAL and saves bandwidth.
- **Malformed input.**
  - Client: bad binary frames (short, wrong length field, unknown type, wrong size) and invalid JSON are dropped and logged, and the stream continues.
  - Server: bad frames and invalid control messages get an `ERROR` reply, and the connection stays up.
- **Cleanup.** `FeedClient.dispose()` closes the socket, clears every timer, removes event listeners, aborts in-flight fetches and cancels pending frames. It is re-entrant, which React StrictMode's double-mount requires. The chart unsubscribes from the store and calls `chart.remove()`.
- **Smooth interaction.** Updates are committed per frame. The chart never re-renders through React. Order-book rows are memoised. Chart gestures (hover, click-to-pin, drag and scroll-zoom) are handled natively by the chart library.

## 10. Debug controls

These are for demonstration only. They are in the **Debug controls** panel and are also available over the WebSocket and REST.

| Control | Effect |
|---|---|
| Force tier: AUTO / FULL / DEGRADED / MINIMAL | Sends `SET_TIER_OVERRIDE`. The effective tier becomes the override. The automatic state machine keeps running, the UI shows what it *would* choose, and AUTO restores it. Also available as `POST /api/debug/clients/:id/tier`. |
| Simulated latency +60/+150/+400 ms | The server delays this connection's PONGs, so the **automatic** tiering can be demonstrated without a bad network. |
| Drop depth / chart packet | The server advances its view of the client without sending one packet. The client detects the gap, re-snapshots and resumes, and the event log shows each step. |
| Kill socket | Closes the socket, then auto-reconnects and resyncs. |
| Go offline / Resume | Stays disconnected, to show the stale state, until resumed. |

## Configuration

All backend settings are environment variables with defaults:

```
SERVER_PORT=8080 (or PORT)   WS_PATH=/ws        SYMBOL=BTCUSDT     RANDOM_SEED=42
ALLOWED_ORIGINS=*            LOG_LEVEL=info|debug
LOG_OUTPUT=file|stdout|both  LOG_DIR=logs  LOG_FORMAT=json|text  LOG_MAX_MB=50  LOG_MAX_DAYS=7
TRADE_INTERVAL_MS=50         DEPTH_SKIP_PCT=15  START_PRICE_CENTS=6500000  TICK_SIZE_CENTS=50
HISTORY_FILE=data/history_1m.json   HISTORY_CANDLES=4320 (cache capacity per interval)
TRADE_BUFFER=20000           STATE_RING=4096
FULL_DEPTH_MS=50      FULL_CHART_MS=100      FULL_TRADE_MS=250
DEGRADED_DEPTH_MS=100 DEGRADED_CHART_MS=250  DEGRADED_TRADE_MS=500
MINIMAL_DEPTH_MS=250  MINIMAL_CHART_MS=1000  MINIMAL_TRADE_MS=2000
WARMUP_SAMPLES=5      REPORT_DEGRADE_MS=3000 REPORT_MINIMAL_MS=6000
```

The frontend reads `NEXT_PUBLIC_API_URL` and, optionally, `NEXT_PUBLIC_WS_URL`. Both are inlined at build time; see `frontend/.env.example`.

## Logging

By default the backend writes **log files instead of printing to the terminal**. The terminal shows one line saying where the logs are.

- **Location:** `LOG_DIR` (default `logs/`, relative to where the server runs). Files are named `backend-YYYY-MM-DD.log`.
- **Format:** JSON lines by default (`LOG_FORMAT=text` gives key=value lines).
- **Rotation:** a new file starts at local midnight. A file over `LOG_MAX_MB` (default 50) is renamed to `backend-YYYY-MM-DD.N.log` and a fresh file is opened. Files older than `LOG_MAX_DAYS` (default 7) are deleted.
- **Contents:** startup, history loading, client connect/disconnect, tier changes with SRTT, RTTVAR and E, overrides, debug actions, sequence resyncs, generator rates every 30 s, one access-log line per REST request (method, path, status, compressed bytes, duration), and panics. `/api/health` is logged only at `LOG_LEVEL=debug`, so platform health checks don't flood the file.
- **Output modes:** `LOG_OUTPUT=file` (local default), `stdout` (the Docker default, so a platform's log viewer captures it) or `both`.

```bash
tail -f backend/logs/backend-$(date +%F).log                  # follow
grep '"client tier changed"' backend/logs/*.log              # tier history
jq -r 'select(.msg=="http request") | [.time,.path,.status,.bytes] | @tsv' backend/logs/*.log
```

## Bandwidth

These figures were measured with a real client against the running backend for 20 s per tier. The payload column is WebSocket message bytes. The wire column adds roughly 80 B per message for WebSocket, TLS and TCP/IP overhead.

| Tier | Server → client msgs/s | Payload | ≈ On the wire | Per hour | Client → server |
|---|---|---|---|---|---|
| FULL | 29 (depth 13.4, chart 10, trades 4, pong + TIER 2) | 3.0 KB/s | 5.3 KB/s | ≈ 19 MB | ≈ 0.12 KB/s |
| DEGRADED | 17 | 1.9 KB/s | 3.3 KB/s | ≈ 12 MB | ≈ 0.12 KB/s |
| MINIMAL | 7 | 0.84 KB/s | 1.4 KB/s | ≈ 5 MB | ≈ 0.12 KB/s |

Depth runs at 13.4/s rather than 20/s at FULL because the server never sends a packet when the book hasn't changed.

**One-time transfers** (REST responses are gzipped):

| Request | JSON | Gzipped |
|---|---|---|
| 3-day 1m history (4,320 candles) | 352 KB | **106 KB** |
| 3-day 5m history (864 candles) | 71 KB | **22 KB** |
| Book snapshot | 0.7 KB | 0.26 KB |
| Frontend JS + CSS, first visit (static host, cached afterwards) | 1.1 MB | ≈ 260 KB |

A first visit costs about 0.4 MB. Each reconnect or interval switch costs about 0.1 MB. After that the stream costs 5–19 MB per hour per open tab, depending on tier. The backend is single-process with low CPU and memory use, so bandwidth is the constraint on free hosting, not compute.

## Hosting (free)

The frontend is fully static; every route is prerendered. The backend must be **one always-running process with WebSockets**, because its canonical market state is in memory: never run more than one instance.

| | Vercel Hobby + Render Free | Vercel Hobby + Oracle Cloud Always Free VM |
|---|---|---|
| Setup effort | ~10 minutes, from Git | ~45 minutes (VM, Docker, firewall) |
| Backend bandwidth | **5 GB/month** (Render Hobby workspace) | **10 TB/month** |
| Backend compute | 512 MB, 0.1 CPU, single instance | Ampere A1: up to 2 OCPU / 12 GB |
| Always on | No: sleeps after **15 min without inbound HTTP/WebSocket traffic**; about 1 min cold start | Yes |
| Behaves like local | Yes while awake. The backend restarts when it wakes: history reloads from the file, live sequences restart, and clients reconnect and resync automatically. | Yes, identical |
| Logs | Render Logs tab (stdout) | Rotating files in `deploy/oracle/logs/`, plus `docker compose logs` |
| Frontend (Vercel Hobby) | 100 GB/month transfer, non-commercial use only | same |

**What 5 GB on Render means:** about 260 hours of one tab at FULL (roughly 8 hours a day) or about 1,000 hours at MINIMAL. That's enough for a demo or interview, but not for leaving tabs open all day. By default the service is **suspended** when the limit is reached; if a payment method is on file, overage is billed at $0.15/GB. For always-on use, or more than a couple of viewers, use the Oracle VM.

### Option A — Vercel + Render (quickest)

1. Push the repository to GitHub.
2. **Backend:** in Render, go to **New → Blueprint**, select the repo, and it reads [`render.yaml`](render.yaml) (Docker, free plan, Singapore region, `/api/health` health check). Set `ALLOWED_ORIGINS` to your Vercel URL, e.g. `https://your-app.vercel.app`. The backend is then at `https://cryptofeed-backend.onrender.com`.
3. **Frontend:** in Vercel, go to **Add New → Project**, choose the repo, set the root directory to `frontend`, and add the environment variable `NEXT_PUBLIC_API_URL=https://cryptofeed-backend.onrender.com`. It is inlined at build time, so redeploy after changing it. The WebSocket URL becomes `wss://…/ws` automatically.
4. Open the Vercel URL. If Render was asleep, the connection badge shows *Reconnecting* for about a minute, then goes live.

### Option B — Vercel + Oracle Always Free VM (always on, like local)

1. Create an Oracle Cloud Always Free account (it needs a card for verification but isn't charged) and an **Ampere A1** Ubuntu VM with 1–2 OCPUs.
2. Open ports 80 and 443 in both places:
   - In the VCN **security list**, add ingress rules for TCP 80 and 443 from `0.0.0.0/0`.
   - On the VM itself, whose image blocks them by default:
     ```bash
     sudo iptables -I INPUT -p tcp -m multiport --dports 80,443 -j ACCEPT && sudo netfilter-persistent save
     ```
3. Install Docker, clone the repo and start the stack:
   ```bash
   curl -fsSL https://get.docker.com | sh
   git clone <your-repo> && cd <repo>/deploy/oracle
   mkdir -p logs && sudo chown 65532:65532 logs      # the container runs as non-root uid 65532
   DOMAIN=<public-ip-with-dashes>.sslip.io ALLOWED_ORIGINS=https://your-app.vercel.app sudo -E docker compose up -d --build
   ```
   [`sslip.io`](https://sslip.io) gives you a free hostname for the IP, and Caddy obtains a Let's Encrypt certificate for it automatically. A domain you own works the same way.
4. **Frontend** on Vercel as in Option A, with `NEXT_PUBLIC_API_URL=https://<public-ip-with-dashes>.sslip.io`.
5. Logs are in `deploy/oracle/logs/backend-YYYY-MM-DD.log`. Updating to a new version is `git pull && sudo -E docker compose up -d --build`.

**Latency and tiers when hosted.** Tiers now reflect the real internet round trip. From India, a Singapore or Mumbai region typically gives an RTT of about 30–80 ms, so the connection should sit at FULL or near the FULL/DEGRADED boundary. A US region usually gives 200 ms or more, which means DEGRADED or MINIMAL. Pick the region closest to your viewers. The debug controls work the same when hosted.

**CI.** `.github/workflows/ci.yml` runs gofmt, vet and `go test -race`, builds the Docker image, runs the frontend typecheck, lint, unit tests and build, and runs the live end-to-end test.

## Tests

| Test | What it proves |
|---|---|
| `backend/internal/client/tier_test.go` | Hysteresis: warmup; a single bad sample doesn't demote; 3 bad samples demote, 3 more reach MINIMAL; 1 good sample doesn't promote; 5 good samples promote; no flapping inside the band; the missing-report policy |
| `backend/internal/candle/candle_test.go` | OHLCV correctness; rollover opens a fresh candle; **coalesced delivery at any cadence reproduces the canonical candles exactly**; base mismatch and eviction |
| `backend/internal/orderbook/orderbook_test.go` | snapshot 100 + deltas 101..103 give the exact book; coalesced deltas across price shifts; gap 100→101→103 detected and recovered |
| `backend/internal/ws/ws_test.go` | Real WebSocket: SYNC, then deltas equal the canonical book; injected gap detected; recovery; override and AUTO; malformed input doesn't kill the connection |
| `backend/internal/history/history_test.go` | 3-day generation is deterministic; save/load round trip; rebase ends at the current minute without changing prices; malformed files rejected (symbol, tick, scale, gaps, alignment, OHLC, volume) |
| `backend/internal/logging/logging_test.go` | Log files rotate by size (.1, .2, …) and at midnight; old files are pruned while unrelated files are kept; writing after close fails |
| `backend/internal/protocol`, `generator` | Exact packet sizes, round trips, malformed packets; determinism by seed; book invariants |
| `frontend/src/lib/__tests__/depthSync.test.ts` | Client sync machine: in-order deltas, coalesced deltas, gap and re-snapshot, **buffering during an in-flight snapshot** (fast path and SYNC fallback), late snapshot ignored, duplicates, disconnect |
| `frontend/src/lib/__tests__/chart.test.ts`, `protocol.test.ts` | Candle rollover from zero, older candle rejected, dedupe, empty history; decoder, malformed packets, the RFC 6298 estimator, fixed-point formatting |
| `frontend/src/lib/__tests__/feed.e2e.test.ts` (opt-in) | The real `FeedClient` against a running backend: local book and candles **byte-identical to REST at the same seq** at FULL and forced MINIMAL, after a gap, after an interval switch and after a reconnect; simulated latency demotes and promotes automatically |

## Packages

- **Backend:** `github.com/gin-gonic/gin` (REST) and `github.com/gorilla/websocket`. Everything else is the Go standard library (`log/slog`, `encoding/binary`, `math/rand`).
- **Frontend:** `next`, `react`, `zustand` (state), `lightweight-charts` (rendering only; the app supplies all data), `tailwindcss` (styling), `vitest` (tests).

## Known limitations

- **Free Render sleeps.** After 15 minutes without traffic the service stops, and waking it takes about 1 minute and restarts the process. Clients recover automatically (reconnect, re-snapshot, and trade-id dedupe is reset per connection), but live sequences restart.
- **Single process, in memory.** Restarting the backend resets live state. The 3-day candle history reloads from the file, but live trades and sequences restart.
- **The history file has candles, not trades.** Three days of individual trades would be about 5 million rows, so `/api/trades` covers only trades generated since startup.
- **History is rebased at startup.** Timestamps shift so the dataset always reads as "the last 3 days"; prices and volumes are unchanged.
- **Trade history is bounded** to the most recent 20,000 trades (about 16 min). Older ranges return what's retained, and `retainedFrom` tells the client where that starts.
- **The live trade list is a window, not a log.** At MINIMAL, trades between flushes appear only in REST history.
- **Short retention for SYNC.** A client whose base is older than the 4096-state ring buffer (about 3.4 min of stalled delivery) gets `RESYNC` and re-snapshots.
- **Book model.** The book is a contiguous tick grid with 10 levels per side, as the packet format requires. Real books can have empty price levels.
- **RTT includes browser main-thread delay.** If the page is busy, measured latency rises. That is arguably correct for an application-level delivery tier.
- **Timestamps come from the backend clock.** Only the event *sequence* is reproducible across runs.
- **Race detector.** `go test -race` needs cgo. It runs in CI on Linux; it could not run on the Windows development machine, which has no C compiler.
- **No watchlist.** There is only one symbol, so the bonus watchlist reordering isn't implemented.
