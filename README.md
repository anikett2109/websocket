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
cd backend && go run ./cmd/gendata          # -days 3 -base 6500000 -tick 50

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

## 0. Design in numbers: every constant and where it comes from

The whole system runs on **one clock**, the market tick **τ = 50 ms** (20 Hz). Seven principles fix every other number:

1. **One clock.** Every delivery interval is a whole number of ticks (k·τ). The hubs flush *on the market tick itself*, so streams that fall due together share one WebSocket frame, and coalescing ratios are exact.
2. **FULL is lossless for depth.** FULL depth is 1τ, so every canonical book state reaches the client.
3. **Chart intervals are geometric.** FULL = 100 ms (10 Hz: about the fastest a person reads discrete price changes, and 20× faster than Binance's 2 s kline stream). MINIMAL = 1000 ms (1 Hz, still twice as fast as Binance's klines). DEGRADED = √(100·1000) ≈ 316, rounded to 6τ = **300 ms**. Each step is ≈3.3×.
4. **Order within a tier.** Depth ≥ chart ≥ trades in update rate. Depth = chart/2, but never less than 1τ. Trades come from the latest-10 window: 10 trades at 20/s span 500 ms.
5. **A tier is eligible if at most one update is in flight.** The pessimistic one-way delay L/2 must not exceed the tier's chart interval T, so **threshold = 2T**: FULL L < 200 ms, DEGRADED L < 600 ms.
6. **Robust statistics.** The app reports latency = median₅(RTT) and jitter = MAD₅(RTT); the server scores L = latency + 4·jitter. The median ignores up to 2 outliers per window; the 4× is RFC 6298's k.
7. **Asymmetric hysteresis.** Demote after 3 reports, promote after 5. Promote at 80% of the threshold (160 / 480 ms): a uniform 20% dead band.

**Generation**, a pure function of the tick n = ⌊(t − 2026-01-01Z)/τ⌋ over a 60 s (1200-tick) cycle:

| Regime | Ticks | Trades | Book changes | Shows |
|---|---|---|---|---|
| Normal | 0–599 (30 s) | 1 / tick = 20/s | every tick | steady flow |
| Burst | 600–799 (10 s) | 3 / tick = **60/s**, ±$60 move | every tick | coalescing, big candles |
| Quiet | 800–1199 (20 s) | 1 / 20 ticks = **1/s** | every 5th tick | no invented updates |
| **Per cycle** | 1200 | **1220 = 20.33/s** | **880 = 14.67/s** | |

**Delivery**, interval in ticks, and exact coalescing:

| Tier | Depth / chart / trades | Trades per chart packet (normal / burst / quiet) | Book states per depth packet | Latest-10 trade coverage |
|---|---|---|---|---|
| FULL | 1τ / 2τ / 5τ = 50 / 100 / 250 ms | 2 / 6 / 1 | 1 (lossless) | 100%, each trade shown twice |
| DEGRADED | 3τ / 6τ / 10τ = 150 / 300 / 500 ms | 6 / 18 / 1 | 3 | **100%, each exactly once** |
| MINIMAL | 10τ / 20τ / 40τ = 500 / 1000 / 2000 ms | 20 / 60 / 1 | 10 | 25% (the REST history page has all) |

**Bandwidth.** Measured per tab over one full 60 s cycle, three clients in parallel. The wire figure adds ≈78 B per frame (WS 2–4 + TLS 22 + TCP/IP 52):

| Tier | Frames/s | Packets/s (depth · chart · trades) | Payload | Wire | Per hour | Model |
|---|---|---|---|---|---|---|
| FULL | 15.8 | 14.67 · 7.0 · 3.0 | 2.62 KB/s | **3.86 KB/s** | 13.9 MB | 3.9 |
| DEGRADED | 8.2 | 5.78 · 2.57 · 1.67 | 1.13 KB/s | **1.77 KB/s** | 6.4 MB | 1.76 |
| MINIMAL | 3.2 | 2.0 · 1.0 · 0.5 | 0.43 KB/s | **0.68 KB/s** | 2.4 MB | 0.66 |

The measured packet rates equal the derivation exactly. For example, FULL chart = (30 s × 10 + 10 s × 10 + 20 s × 1)/60 = 7.0/s, and FULL depth = 880/60 = 14.67/s, so FULL is lossless. **Bandwidth depends on the tier, not on market activity**: packets are fixed-size and a burst only raises coalescing.

**Server location.** RTT_min ≈ 2·d·r / (200 km/ms) + s, where fibre carries light at ≈ ⅔c, route inflation r ≈ 2 and platform overhead s ≈ 10 ms.

| From India to | Distance | RTT (measured / estimated) | L | Tier |
|---|---|---|---|---|
| Singapore (this deployment) | 3,160 km | **77–125 ms, median 89 (measured)** | ≈ 108 | FULL |
| Frankfurt | ~6,300 km | ≈ 130–185 ms | ≈ 170 | FULL/DEGRADED border |
| US West | ~13,000 km | ≈ 230–295 ms | ≈ 280 | DEGRADED |
| GEO satellite | — | ≈ 600 ms+ | ≥ 600 | MINIMAL |

FULL needs RTT ≲ 180 ms, i.e. a server within ~8,500 km. Every terrestrial path fits inside DEGRADED; even the antipode is ≈ 410 ms. **MINIMAL means impairment** (congestion, lossy wireless, satellite), never geography alone.

**Hysteresis behaviour** (`tier_test.go`, 1 probe/s):

| Scenario | Result |
|---|---|
| Healthy India → Singapore | FULL after the 3-probe warmup, and stays FULL |
| 1 or 2 spikes of 900 ms | **No change** |
| Severe congestion (+400–600 ms) | DEGRADED on the 5th bad probe, MINIMAL within 10; after recovery, DEGRADED at +7 s and FULL at +12 s |
| Flapping 3 s bad / 3 s good | ≤ 5 changes (promoting after 3 reports instead of 5 gives 21) |

**Free-tier budget** (Render Hobby: 5 GB/month = 1.93 KB/s sustained): about **360 tab-hours at FULL**, 780 at DEGRADED and 2,080 at MINIMAL. That's roughly 0.5, 1.1 or 2.9 tabs open 24/7.

## 1. Architecture

```
 Generator ──(chan Event, one per 50 ms tick)──► Market processor  (single writer goroutine)
   pure function of tick n                    ├─ candle.Series 1m ┐ ring of canonical states
   (no RNG, restart-safe)                     ├─ candle.Series 5m ┘ keyed by tick
                                              ├─ trades.Store (ring, 20k)
                                              └─ orderbook.Engine ── ring of books keyed by tick
                                                         │  RWMutex reads · publishes tick n
                    ┌────────────────────────────────────┼─────────────────────────────┐
                    ▼                                    ▼                             ▼
          REST (Gin) /api/*                  ws.Server = client manager         3 tier hubs
          history · snapshots · trades       conn lifecycle · tier state        FULL / DEGRADED / MINIMAL
                                             machine · overrides ──moves──►     flush on tick n when n % k == 0
                                                                                one writer goroutine per conn
```

**What happened vs. when it's delivered.** The market processor applies every generated event, whatever tier the clients are on. The WebSocket layer never computes candles and never owns the book. It reads canonical state and decides when each client receives it. A slower tier only changes how often updates are delivered. It never changes the final OHLCV values.

**Tier hubs.** There is one WebSocket per browser. The client manager puts each connection into one of three hubs. A hub has no timers of its own: after the market processes tick n it publishes n, and each stream fires when n is a multiple of its interval in ticks. Everything due on that tick goes into **one binary frame**; the header length field delimits the packets. Clients flushed on the same tick converge on the same base sequence, so each packet is encoded once per `(stream, base)` and shared across those clients. A client that just moved to a new hub gets one catch-up delta computed from its own last sequence. After that it is in step with the rest of the hub.

**Backpressure.** Sends never block. If a client's buffer is full, that flush is skipped and the client's last-sent sequence doesn't advance, so the next flush sends one larger coalesced delta. Data is never silently lost.

### Backend packages

| Package | Responsibility |
|---|---|
| `generator` | The market as a pure function of the tick: regime schedule, closed-form trade ids and sequences, price path, book, candle aggregation |
| `market` | Canonical state; single writer; consistent read views for REST and the hubs |
| `candle` | Per-interval OHLCV, a ring of states by `chartSeq`, and coalesced `Transitions(base)` |
| `orderbook` | Authoritative book, `depthSeq`, a state ring, and `Diff`/`ApplyDelta` |
| `trades` | Bounded ring buffer: the latest 10 plus time-range queries |
| `history` | History file (sampled from the same function): generate, save, load, validate, and fill up to "now" |
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
src/lib/latency.ts         HealthMeter (latency = median5, jitter = MAD5), rate counters
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

Everything is a **pure function of the market tick** n = ⌊(t − 2026-01-01T00:00Z) / 50 ms⌋ (`internal/generator`). There is no random number generator and no hidden state, so the feed is exactly repeatable. A restarted process resumes the **same trade ids, sequences and prices**, and history and live data are one continuous series.

- **Trades** (see the regime table in §0): trade j of tick n has id `TradesBefore(n) + j + 1`, computed in closed form (1220 per cycle), and `ts = tick start + j·⌊50/m⌋ ms`, so ids and timestamps are strictly increasing. Even ids buy at the ask; odd ids sell at the bid. Size cycles through 13 values from 0.010 to 0.058 BTC.
- **Price path:** a sum of four sine waves, snapped to the 0.50 grid, with amplitude/period pairs of $250/6 h (multi-day trend), $120/97 min, $30/4 min (1m candle bodies) and $8/37 s. Each burst adds a ±$60 triangular move (the direction alternates by cycle) plus a ±$15 2 s wiggle. Both are zero at the window edges, so the path is continuous. The maximum slope is ≈ 2.3 $/s, which gives 1m ranges of about $20–90.
- **Book:** 10 levels per side on a contiguous 0.50 grid with a one-tick spread. Each level pulses between its floor (0.20 + 0.10·i BTC) and floor + 0.30 with a 2 s period, phase-shifted per level. It changes every tick in normal and burst and every 5th tick in quiet.
- **History file:** `backend/data/history_1m.json` holds **3 days of 1m candles** (4,320 rows, about 250 KB), sampled from the same function by `go run ./cmd/gendata`.
  - **Format:** a JSON header (symbol, scales, tick, base price, generator `tick-v1`), then one `[t, o, h, l, c, v]` row per line.
  - **At startup** the server validates the file: symbol, scales and market model must match; candles must be contiguous, minute-aligned and consistent. It keeps the file's candles inside the 3-day window and **computes any minutes the file doesn't cover** from the same function, up to and including the partial current minute. A file generated hours earlier therefore still yields exactly the candles a never-stopped server would have (`TestWindowEqualsFunction`).
  - **The trade ring** is pre-filled with the last 20,000 ticks of trades, so `/api/trades` and the latest-10 panel are populated immediately.
  - **Missing file:** everything is computed, with a warning. A file that exists but is invalid is a startup error.
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
| `GET /api/ws/status` | per-connection tier, reported latency and jitter, score L, override, sequences, packets, frames and bytes sent |
| `POST /api/debug/clients/:id/tier` `{"tier":"AUTO\|FULL\|DEGRADED\|MINIMAL"}` | debug override |

Invalid intervals, limits, ranges and symbols return `400`/`404` with `{"error": "..."}`. Empty history returns `candles: []` and `active: null`.

## 5. WebSocket protocol

Market data is binary, little-endian, with a 15-byte header on every packet. **One WebSocket frame carries every packet due on the same tick**: split it using each header's `length`.

```
type u8 | length u16 (whole packet) | seq u32 | timestamp i64
```

| Type | Size | Header `seq` / `ts` | Payload |
|---|---|---|---|
| 1 `CHART_DELTA` | **63 B** | chart seq (tick) / candle start | `baseSeq` (i64 slot), Δopen, Δhigh, Δlow, Δclose, Δvolume (i64) |
| 2 `DEPTH_DELTA` | **115 B** | depth seq (tick) / send time | `baseSeq` u32, `bestBid` i64, `bestAsk` i64, 10 bid Δqty i32, 10 ask Δqty i32 |
| 3 `TRADE_UPDATE` | **143 B** | newest trade id / newest trade ts | LTP i64, then 10 × (Δprice i32, ±qty i32, Δtime i32) |
| 4 `PING` | **15 B** | probe seq / client clock (µs) | none (client → server) |
| 5 `PONG` | **19 B** | the same values echoed | server hold u32 µs: time the server kept the probe beyond any simulated delay |

Design notes:

- **`baseSeq` in CHART_DELTA** occupies the slot originally planned for `ltpDelta`. That field is redundant because the active candle's close always equals the LTP, so reusing the slot keeps the packet at 63 B.
- **`bestBid`/`bestAsk` in DEPTH_DELTA** make the packet self-describing when the book shifts price. Level `i` sits at `bestBid − i·tick` / `bestAsk + i·tick`. Each quantity delta is relative to the client's quantity *at that same price* (0 if the client had no level there), so a shifted book re-indexes naturally. A level whose quantity reaches zero is removed.
- **TRADE_UPDATE** carries the latest 10 trades, newest first. The trade ids are consecutive (`seq`, `seq−1`, … `seq−9`), so every trade keeps its unambiguous ordering id without extra bytes. The sign of the quantity encodes the aggressor side (+ buy, − sell). Price and time are deltas from the packet's LTP and timestamp.

JSON text frames carry control messages:

| Direction | Message |
|---|---|
| server → client | `HELLO {connId, scales, tickSize, tiers, clock}`, `TIER {tier, autoTier, override, reason, rates, effectiveLatencyMs (= L), medianMs, madMs, …}` (on change plus every 5 s), `SUBSCRIBED`, `SYNCED {stream, seq}`, `SYNC_FAILED`, `RESYNC {stream}`, `ERROR`, `DEBUG_ACK` |
| client → server | `SUBSCRIBE {symbol, interval}`, `SYNC {stream, seq, interval?}`, `NET_REPORT {rttMs, srttMs, rttvarMs, samples}`, `SET_TIER_OVERRIDE {tier}`, `DEBUG {action: DROP_DEPTH\|DROP_CHART\|SIM_LATENCY\|SPIKE, ms}` |

## 6. Sequences and synchronisation (chart and order book)

There is **one clock for every stream: the market tick.** A stream's sequence is the tick at which its canonical state last changed:

- chart seq = the tick of the last trade applied (the same value for 1m and 5m);
- depth seq = the tick of the last book change;
- trades keep their own ids (`TRADE_UPDATE`'s header seq is the newest id).

A tier only decides *which ticks* are sampled. With the market in its normal regime:

```
FULL     chart: base 100 → 102 → 104 → 106 …     (every 2 ticks)
DEGRADED chart: base 100 → 106 → 112 → 118 …     (every 6 ticks)
MINIMAL  chart: base 100 → 120 → 140 → 160 …     (every 20 ticks)
```

When nothing changed (the quiet regime between trades) no packet is sent, so the next base is the last *delivered* tick, not seq − k.

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

**Server retention.** Ring buffers hold 4096 ticks per stream (about 3.4 min). If a client's base has aged out, the server sends `RESYNC` or `SYNC_FAILED` and the client re-snapshots.

## 7. Health report: latency and jitter measurement

The app measures and reports; the backend decides (task.txt §3).

1. **Probe.** The app sends a binary `PING(probeSeq, performance.now() µs)` and the server echoes it immediately as a `PONG`. It starts with **3 fast-start probes 200 ms apart**, then sends 1 per second. The network RTT = `now − echoedTimestamp − serverHold` uses only the browser's monotonic clock. The server reports how long it held the probe beyond the intentional simulated delay (CPU scheduling, late timers), and the app subtracts it, as NTP subtracts server processing time: delay = (t4 − t1) − (t3 − t2). **The tier reflects the client's network, not the server's CPU.** A probe is discarded if its seq is in the future, if its RTT is negative or over 60 s, or if it arrives while the tab is hidden (throttled timers would be measured instead of the network).
2. **Health report.** Over the last **W = 5 RTTs**:
   - **latency = median(window)**: the typical round trip.
   - **jitter = MAD = median(|RTTᵢ − latency|)**: the typical deviation.

   The app sends `NET_REPORT {latencyMs, jitterMs, rttMs, samples}` after every probe (`frontend/src/lib/latency.ts`, `HealthMeter`).
3. **Decision (backend).** The server validates both values (finite, 0 to 60 s) and scores **L = latency + 4·jitter**: a pessimistic round-trip bound, with k = 4 as in RFC 6298. See §8 for the thresholds.

**Why median and MAD rather than RFC 6298's SRTT/RTTVAR.** Both robust statistics tolerate up to 2 outliers in 5, so a latency spike moves neither. With EWMA smoothing, one 900 ms spike inflates 4·RTTVAR for several reports: in simulation it demoted FULL→MINIMAL, and SRTT (α = 1/8) needs about 8 samples to converge, so recovery took 28–35 s. A real change of level is picked up within 3 probes (when 3 of the 5 samples move).

**Worked example (production, India → Singapore).** RTTs [89, 90, 88, 118, 91] → sorted [88, 89, 90, 91, 118] → latency = **90**. Deviations [1, 0, 2, 28, 1] → sorted [0, 1, 1, 2, 28] → jitter = **1**. So L = 90 + 4·1 = **94 ms** → FULL. The 118 ms spike is ignored.

## 8. Tiers, thresholds and hysteresis

| Tier | L range (L = reported latency + 4·jitter) | Depth | Chart | Trades |
|---|---|---|---|---|
| **FULL** | L < 200 ms | 50 ms (1τ, lossless) | 100 ms (2τ) | 250 ms (5τ) |
| **DEGRADED** | 200 ≤ L < 600 ms | 150 ms (3τ) | 300 ms (6τ) | 500 ms (10τ) |
| **MINIMAL** | L ≥ 600 ms | 500 ms (10τ) | 1000 ms (20τ) | 2000 ms (40τ) |

The derivation of every number is in [§0](#0-design-in-numbers-every-constant-and-where-it-comes-from). In short: the chart intervals are geometric (100 → 300 → 1000); each threshold = 2 × the tier's chart interval (at most one update in flight); depth = chart/2; trades follow the latest-10 window.

**Hysteresis** (`internal/client/tier.go`). Transitions move one step at a time:

| Transition | Condition |
|---|---|
| FULL → DEGRADED | L ≥ 200 ms for **3** consecutive reports |
| DEGRADED → MINIMAL | L ≥ 600 ms for **3** consecutive reports |
| MINIMAL → DEGRADED | L < 480 ms for **5** consecutive reports |
| DEGRADED → FULL | L < 160 ms for **5** consecutive reports |

- **Warmup:** a connection starts in DEGRADED and is classified directly after 3 probes.
- **Missing reports:** silence counts as bad samples. After 3 s (3 missed probes, the demotion count) the tier is capped at DEGRADED; after 6 s it drops to MINIMAL.
- **Connection drops:** the reader exits, and the connection leaves its hub with its state released. A WebSocket control ping every 15 s with a 45 s read deadline detects dead TCP connections.
- **Tier changes never create a new sequence space.** The first packet in the new hub is a catch-up delta from the client's own last tick.
- **Candle correctness across tiers:** `candle_test.go` (tick-boundary flushes at any cadence reproduce the canonical candles exactly), `TestHubBatchesAndCoalescesByTick` (a DEGRADED frame carries chart and depth `n → n+6` plus the trades in one frame, and nothing is sent when nothing changed), and the e2e test at forced MINIMAL.
- **Display:** the tier (automatic or forced) and reason, target vs. received rates per stream, the last RTT, the reported latency and jitter, the server's score L, the threshold bands, and the current market regime with a countdown.

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
| Simulated latency: off / **+250 ms → DEGRADED** / **+700 ms → MINIMAL** | The server delays this connection's PONGs so the **automatic** tiering reacts. With the measured ~90 ms base, +250 puts L near the middle of the DEGRADED band (≈ 340–400). That leaves room below 600 for the timer lateness of a 0.1-CPU host (see Known limitations). |
| **Spike (tier should hold)** | Delays exactly one PONG by 900 ms. The median ignores it, which shows the hysteresis working. |
| Drop depth / chart packet | The server advances its view of the client without sending one packet. The client detects the gap, re-snapshots and resumes, and the event log shows each step. |
| Kill socket | Closes the socket, then auto-reconnects and resyncs. |
| Go offline / Resume | Stays disconnected, to show the stale state, until resumed. |

## Configuration

All backend settings are environment variables with defaults:

```
SERVER_PORT=8080 (or PORT)   WS_PATH=/ws        SYMBOL=BTCUSDT
ALLOWED_ORIGINS=*            LOG_LEVEL=info|debug
LOG_OUTPUT=file|stdout|both  LOG_DIR=logs  LOG_FORMAT=json|text  LOG_MAX_MB=50  LOG_MAX_DAYS=7
BASE_PRICE_CENTS=6500000     TICK_SIZE_CENTS=50   (the market tick itself is fixed at 50 ms)
HISTORY_FILE=data/history_1m.json   HISTORY_CANDLES=4320 (cache capacity per interval)
TRADE_BUFFER=20000           STATE_RING=4096
FULL_DEPTH_MS=50      FULL_CHART_MS=100      FULL_TRADE_MS=250
DEGRADED_DEPTH_MS=150 DEGRADED_CHART_MS=300  DEGRADED_TRADE_MS=500
MINIMAL_DEPTH_MS=500  MINIMAL_CHART_MS=1000  MINIMAL_TRADE_MS=2000
WARMUP_SAMPLES=3      REPORT_DEGRADE_MS=3000 REPORT_MINIMAL_MS=6000
(delivery intervals are rounded down to whole 50 ms ticks)
```

The frontend reads `NEXT_PUBLIC_API_URL` and, optionally, `NEXT_PUBLIC_WS_URL`. Both are inlined at build time; see `frontend/.env.example`.

## Logging

By default the backend writes **log files instead of printing to the terminal**. The terminal shows one line saying where the logs are.

- **Location:** `LOG_DIR` (default `logs/`, relative to where the server runs). Files are named `backend-YYYY-MM-DD.log`.
- **Format:** JSON lines by default (`LOG_FORMAT=text` gives key=value lines).
- **Rotation:** a new file starts at local midnight. A file over `LOG_MAX_MB` (default 50) is renamed to `backend-YYYY-MM-DD.N.log` and a fresh file is opened. Files older than `LOG_MAX_DAYS` (default 7) are deleted.
- **Contents:** startup, history loading, client connect/disconnect, tier changes with the score L, overrides, debug actions, sequence resyncs, generator rates every 30 s, one access-log line per REST request (method, path, status, compressed bytes, duration), and panics. `/api/health` is logged only at `LOG_LEVEL=debug`, so platform health checks don't flood the file.
- **Output modes:** `LOG_OUTPUT=file` (local default), `stdout` (the Docker default, so a platform's log viewer captures it) or `both`.

```bash
tail -f backend/logs/backend-$(date +%F).log                  # follow
grep '"client tier changed"' backend/logs/*.log              # tier history
jq -r 'select(.msg=="http request") | [.time,.path,.status,.bytes] | @tsv' backend/logs/*.log
```

## Bandwidth

See the measured per-tier table in [§0](#0-design-in-numbers-every-constant-and-where-it-comes-from): FULL 3.86, DEGRADED 1.77 and MINIMAL 0.68 KB/s on the wire, averaged over one 60 s market cycle. Before the redesign (independent timers, TIER every second, one packet per frame) the same tiers used 5.3, 3.3 and 1.4 KB/s. Uplink is ≈ 0.11 KB/s of payload per tab (PING plus NET_REPORT).

**One-time transfers** (REST responses are gzipped):

| Request | JSON | Gzipped |
|---|---|---|
| 3-day 1m history (4,320 candles) | 352 KB | **106 KB** |
| 3-day 5m history (864 candles) | 71 KB | **22 KB** |
| Book snapshot | 0.7 KB | 0.26 KB |
| Frontend JS + CSS, first visit (static host, cached afterwards) | 1.1 MB | ≈ 260 KB |

A first visit costs about 0.4 MB. Each reconnect or interval switch costs about 0.1 MB. After that the stream costs 2.4–13.9 MB per hour per open tab, depending on tier. The backend is single-process with low CPU and memory use, so bandwidth is the constraint on free hosting, not compute.

## Hosting (free)

The frontend is fully static; every route is prerendered. The backend must be **one always-running process with WebSockets**, because its canonical market state is in memory: never run more than one instance.

| | Vercel Hobby + Render Free | Vercel Hobby + Oracle Cloud Always Free VM |
|---|---|---|
| Setup effort | ~10 minutes, from Git | ~45 minutes (VM, Docker, firewall) |
| Backend bandwidth | **5 GB/month** (Render Hobby workspace) | **10 TB/month** |
| Backend compute | 512 MB, 0.1 CPU, single instance | Ampere A1: up to 2 OCPU / 12 GB |
| Always on | No: sleeps after **15 min without inbound HTTP/WebSocket traffic**; about 1 min cold start | Yes |
| Behaves like local | Yes while awake. Waking restarts the process, but the market is a function of the clock, so prices, trade ids and sequences resume exactly where they would have been; clients reconnect and resync automatically. | Yes, identical |
| Logs | Render Logs tab (stdout) | Rotating files in `deploy/oracle/logs/`, plus `docker compose logs` |
| Frontend (Vercel Hobby) | 100 GB/month transfer, non-commercial use only | same |

**What 5 GB on Render means:** about 360 hours of one tab at FULL (roughly 12 hours a day) or about 2,080 hours at MINIMAL. That's enough for a demo or interview, but not for leaving tabs open all day. By default the service is **suspended** when the limit is reached; if a payment method is on file, overage is billed at $0.15/GB. For always-on use, or more than a couple of viewers, use the Oracle VM.

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

**Latency and tiers when hosted.** Tiers now reflect the real internet round trip. From India to Singapore we measured 77–125 ms (L ≈ 108), which is FULL. The location table in §0 derives the ceiling for other regions: Frankfurt is borderline and the US is DEGRADED. Pick the region closest to your viewers. The debug controls work the same when hosted.

**CI.** `.github/workflows/ci.yml` runs gofmt, vet and `go test -race`, builds the Docker image, runs the frontend typecheck, lint, unit tests and build, and runs the live end-to-end test.

## Tests

| Test | What it proves |
|---|---|
| `backend/internal/generator/generator_test.go` | Regime schedule; per-cycle totals (1220 trades, 880 book states); **closed forms equal plain counting** over 5 cycles; the event at tick n is a pure function (restart-safe); ids and timestamps increasing; trades at the touch; price continuous across regimes |
| `backend/internal/client/tier_test.go` | Score (median/MAD); healthy India→Singapore reaches FULL and stays; **1 or 2 spikes cause no change**; severe congestion demotes and recovers on schedule; moderate congestion leaves FULL; flapping ≤ 5 changes; dead band holds; location profiles (20 ms → GEO satellite); missing-report policy; thresholds = 2 × chart interval |
| `backend/internal/ws/ws_test.go` | **`TestHubBatchesAndCoalescesByTick`** (the sequence model: DEGRADED frame = depth + chart `n→n+6` + trades, nothing sent when nothing changed); real WebSocket SYNC with deltas matching the market function; injected gap and recovery; override and AUTO; malformed input |
| `backend/internal/candle/candle_test.go` | OHLCV; rollover; coalesced delivery at any tick cadence (3 trades per tick) reproduces the canonical candles; base mismatch; eviction |
| `backend/internal/orderbook/orderbook_test.go` | Snapshot + deltas give the exact book; coalescing at 1/3/10 ticks across price shifts and the quiet regime; gap detection; out-of-order book rejected |
| `backend/internal/history/history_test.go` | Save/load round trip; **an old file plus computed fill equals the pure function**, including the partial current minute; malformed files rejected |
| `backend/internal/protocol`, `logging` | Packet sizes, round trips, malformed input, frame splitting; log rotation and retention |
| `frontend/src/lib/__tests__/*.test.ts` | Sync state machine (buffering during snapshot, fast path, SYNC fallback, gaps, late responses); chart rollover; decoder and **batched-frame splitting**; RFC 6298 estimator; fixed-point formatting |
| `frontend/src/lib/__tests__/feed.e2e.test.ts` (opt-in) | The real `FeedClient` against a running backend: book and candles byte-identical to REST at the same seq at FULL and forced MINIMAL, after a gap, an interval switch and a reconnect; **a spike does not change the tier**; +700 ms demotes to MINIMAL and removing it promotes back to FULL |

## Packages

- **Backend:** `github.com/gin-gonic/gin` (REST) and `github.com/gorilla/websocket`. Everything else is the Go standard library (`log/slog`, `encoding/binary`, `math`).
- **Frontend:** `next`, `react`, `zustand` (state), `lightweight-charts` (rendering only; the app supplies all data), `tailwindcss` (styling), `vitest` (tests).

## Known limitations

- **Free Render sleeps.** After 15 minutes without traffic the service stops, and waking takes about 1 minute. Because the market is a function of the clock, the restarted process continues the same series; clients reconnect and resync.
- **Single process, in memory.** Only one backend instance may run, because hubs sample that process's canonical state.
- **Synthetic, periodic market.** The price is a sum of sines and the regimes repeat every 60 s. That's ideal for a repeatable demonstration, but visibly regular.
- **Trade history is bounded** to the last 20,000 ticks of trades (pre-filled at startup). Older trades can be recomputed from the function but aren't served.
- **Trade history is bounded** to the most recent 20,000 trades (about 16 min). Older ranges return what's retained, and `retainedFrom` tells the client where that starts.
- **The live trade list is a window, not a log.** At MINIMAL, trades between flushes appear only in REST history.
- **Short retention for SYNC.** A client whose base is older than the 4096-state ring buffer (about 3.4 min of stalled delivery) gets `RESYNC` and re-snapshots.
- **Book model.** The book is a contiguous tick grid with 10 levels per side, as the packet format requires. Real books can have empty price levels.
- **RTT includes browser main-thread delay.** If the page is busy, measured latency rises. That is arguably correct for an application-level delivery tier.
- **Timestamps come from the backend clock.** Timestamps, ids and prices are all derived from it, so a skewed server clock shifts the whole market.
- **Small hosts pause the server.** On Render's free 0.1 CPU the process is paused whenever its CPU quota runs out. Delayed PONG timers then fired up to ~400 ms late, which made a +250 ms client flap between DEGRADED and MINIMAL. The PONG now carries that hold time and the app subtracts it. What remains uncompensated is time a PING waits in the kernel before a paused process reads it, which is usually small.
- **Race detector.** `go test -race` needs cgo. It runs in CI on Linux; it could not run on the Windows development machine, which has no C compiler.
- **No watchlist.** There is only one symbol, so the bonus watchlist reordering isn't implemented.
