// FeedClient owns the WebSocket, REST snapshots, both synchronisers, latency
// probes, reconnection and browser-lifecycle handling. It is framework-free;
// React only sees its output through the store (committed once per frame).
import { api, HISTORY_LIMIT, type BookSnapshot, type CandlesResponse } from "./api";
import { HealthMeter, RateCounter } from "./latency";
import { decodeFrame, encodePing, type ChartDelta, type DepthDelta, type Packet, type TradeUpdate } from "./protocol";
import { applyDepthDelta, type BookState } from "./sync/book";
import { applyChartDelta, normalizeCandles, type ChartState } from "./sync/chart";
import { StreamSync } from "./sync/streamSync";
import { commit, initialState, useMarket, type LogEvent, type MarketState, type Override, type Patch, type Rates, type TierName } from "@/store/market";

const PING_EVERY_MS = 1000;
const FAST_START_MS = [200, 400];
const TICKER_EVERY_MS = 30_000;
const MAX_BACKOFF_MS = 10_000;

type ServerMsg = { type: string; [k: string]: unknown };

export class FeedClient {
  private ws: WebSocket | null = null;
  private disposed = false;
  private paused = false;
  private attempt = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private pingTimer: ReturnType<typeof setInterval> | null = null;
  private statsTimer: ReturnType<typeof setInterval> | null = null;
  private tickerTimer: ReturnType<typeof setInterval> | null = null;
  private retryTimers = new Set<ReturnType<typeof setTimeout>>();

  private tick = 50;
  private probeSeq = 0;
  private meter = new HealthMeter();
  private rates = { chart: new RateCounter(), depth: new RateCounter(), trades: new RateCounter() };

  private chartInterval = initialState.chart.interval;
  private awaitingSubAck = false;
  private chartResetToken = 0;
  private bookAbort: AbortController | null = null;
  private chartAbort: AbortController | null = null;
  private tickerAbort: AbortController | null = null;

  private lastTradeId = 0;
  private ltpSeq = 0;
  private ltp: number | null = null;

  private pending: Patch = {};
  private events: LogEvent[] = [];
  private eventsDirty = false;
  private eventId = 0;
  private frame: number | null = null;

  readonly depth: StreamSync<BookState, DepthDelta>;
  readonly chart: StreamSync<ChartState, ChartDelta>;

  constructor(private readonly url: string) {
    this.depth = new StreamSync<BookState, DepthDelta>("book", (s, d) => applyDepthDelta(s, d, this.tick), {
      requestSnapshot: (token) => this.fetchBook(token),
      sendSync: (seq) => this.send({ type: "SYNC", stream: "depth", seq }),
      onState: (s) => this.queue({ book: { bids: s.bids, asks: s.asks, seq: s.seq, updatedAt: Date.now() } }),
      onPhase: (phase) => this.queue({ book: { phase } }),
      onEvent: (m, l) => this.log(m, l),
    });
    this.chart = new StreamSync<ChartState, ChartDelta>("chart", applyChartDelta, {
      requestSnapshot: (token) => this.fetchCandles(token),
      sendSync: (seq) => this.send({ type: "SYNC", stream: "chart", interval: this.chartInterval, seq }),
      onState: (s) => {
        this.queue({ chart: { closed: s.closed, active: s.active, seq: s.seq } });
        if (s.active) this.updateLtp(s.seq, s.active.c);
      },
      onPhase: (phase) => this.queue({ chart: { phase } }),
      onEvent: (m, l) => this.log(m, l),
    });
  }

  // ---------------------------------------------------------------- lifecycle

  start() {
    this.disposed = false;
    this.connect();
    document.addEventListener("visibilitychange", this.onVisibility);
    window.addEventListener("online", this.onOnline);
    window.addEventListener("offline", this.onOffline);
    window.addEventListener("pagehide", this.onPageHide);
    window.addEventListener("pageshow", this.onPageShow);
    this.statsTimer = setInterval(() => this.publishRates(), 1000);
    this.tickerTimer = setInterval(() => this.fetchTicker(), TICKER_EVERY_MS);
  }

  dispose() {
    this.disposed = true;
    document.removeEventListener("visibilitychange", this.onVisibility);
    window.removeEventListener("online", this.onOnline);
    window.removeEventListener("offline", this.onOffline);
    window.removeEventListener("pagehide", this.onPageHide);
    window.removeEventListener("pageshow", this.onPageShow);
    for (const t of [this.statsTimer, this.tickerTimer, this.pingTimer]) if (t) clearInterval(t);
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    this.retryTimers.forEach(clearTimeout);
    this.bookAbort?.abort();
    this.chartAbort?.abort();
    this.tickerAbort?.abort();
    if (this.frame !== null) cancelAnimationFrame(this.frame);
    this.frame = null;
    this.depth.stop();
    this.chart.stop();
    const ws = this.ws;
    this.ws = null;
    ws?.close();
  }

  private connect() {
    if (this.disposed || this.paused) return;
    this.reconnectTimer = null;
    this.queue({ conn: { status: this.attempt === 0 ? "connecting" : "reconnecting", reconnectAt: null, attempt: this.attempt } });
    let ws: WebSocket;
    try {
      ws = new WebSocket(this.url);
    } catch (e) {
      this.log(`websocket error: ${(e as Error).message}`, "warn");
      this.scheduleReconnect();
      return;
    }
    ws.binaryType = "arraybuffer";
    this.ws = ws;
    ws.onmessage = (ev) => {
      if (this.ws !== ws) return; // message from a superseded socket
      this.queue({ conn: { lastMessageAt: Date.now() } });
      if (typeof ev.data === "string") this.onText(ev.data);
      else this.onBinary(ev.data as ArrayBuffer);
    };
    ws.onclose = () => {
      if (this.ws === ws) this.onClosed();
    };
  }

  private onClosed() {
    this.ws = null;
    if (this.pingTimer) clearInterval(this.pingTimer);
    this.pingTimer = null;
    this.depth.stop();
    this.chart.stop();
    this.bookAbort?.abort();
    this.chartAbort?.abort();
    this.meter.reset();
    this.log(this.paused ? "disconnected (paused by debug control)" : "disconnected", "warn");
    this.queue({
      conn: { status: this.paused ? "offline" : "reconnecting", disconnectedAt: Date.now(), connId: null },
      tier: { tier: null },
    });
    if (!this.disposed && !this.paused) this.scheduleReconnect();
  }

  private scheduleReconnect() {
    const base = Math.min(MAX_BACKOFF_MS, 500 * 2 ** this.attempt);
    const delay = Math.round(base * (0.7 + Math.random() * 0.6)); // jitter avoids thundering herds
    this.attempt++;
    this.queue({ conn: { reconnectAt: Date.now() + delay, attempt: this.attempt } });
    this.reconnectTimer = setTimeout(() => this.connect(), delay);
  }

  private reconnectNow(reason: string) {
    if (this.disposed || this.paused || this.ws) return;
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    this.log(`reconnecting now (${reason})`, "info");
    this.attempt = 0;
    this.connect();
  }

  private onVisibility = () => {
    if (document.visibilityState === "visible") {
      // Samples taken while hidden measured timer throttling, not the network.
      this.meter.reset();
      this.log("tab visible: latency estimator reset", "info");
      this.reconnectNow("tab visible");
      this.scheduleFlush();
    } else {
      // Deltas keep being applied while hidden (they are cheap); rendering pauses
      // because commits are frame-driven.
      // Probes are not reported while hidden, so the server's missing-report
      // policy moves a background tab to MINIMAL (saving bandwidth); it is
      // promoted again through normal hysteresis once visible.
      this.log("tab hidden: rendering paused, sync continues, latency reports paused", "info");
    }
  };
  // Close cleanly when the page is unloaded or enters the back/forward cache,
  // and reconnect if it is restored from there.
  private onPageHide = () => this.ws?.close(1001, "page hidden");
  private onPageShow = (e: PageTransitionEvent) => {
    if (e.persisted) this.reconnectNow("restored from bfcache");
  };
  private onOnline = () => this.reconnectNow("network online");
  private onOffline = () => this.log("browser reports offline", "warn");

  // ---------------------------------------------------------------- messages

  private send(msg: object | ArrayBuffer) {
    if (this.ws?.readyState !== WebSocket.OPEN) return false;
    this.ws.send(msg instanceof ArrayBuffer ? msg : JSON.stringify(msg));
    return true;
  }

  private onText(raw: string) {
    let m: ServerMsg;
    try {
      m = JSON.parse(raw);
      if (!m || typeof m.type !== "string") throw new Error("missing type");
    } catch {
      this.log("malformed control message ignored", "warn");
      return;
    }
    switch (m.type) {
      case "HELLO":
        return this.onHello(m);
      case "TIER":
        return this.onTier(m);
      case "SUBSCRIBED":
        if (m.interval === this.chartInterval) this.awaitingSubAck = false;
        return;
      case "SYNCED":
        if (m.stream === "depth") this.depth.onSynced(Number(m.seq));
        else if (m.stream === "chart" && m.interval === this.chartInterval) this.chart.onSynced(Number(m.seq));
        return;
      case "SYNC_FAILED":
        (m.stream === "depth" ? this.depth : this.chart).onSyncFailed(String(m.reason));
        return;
      case "RESYNC":
        (m.stream === "depth" ? this.depth : this.chart).onResync();
        return;
      case "DEBUG_ACK":
        this.log(`debug ${m.action} acknowledged`, "info");
        return;
      case "ERROR":
        this.log(`server error: ${m.message}`, "warn");
        return;
      default:
        this.log(`unknown message type ${m.type}`, "warn");
    }
  }

  private onBinary(buf: ArrayBuffer) {
    const { packets, error } = decodeFrame(buf);
    for (const p of packets) this.onPacket(p);
    if (error) this.log(`malformed packet dropped: ${error}`, "warn");
  }

  private onPacket(p: Packet) {
    const now = performance.now();
    switch (p.kind) {
      case "depth":
        this.rates.depth.hit(now);
        this.depth.onDelta(p);
        return;
      case "chart":
        this.rates.chart.hit(now);
        if (!this.awaitingSubAck) this.chart.onDelta(p); // earlier packets belong to the old interval
        return;
      case "trades":
        this.rates.trades.hit(now);
        return this.onTrades(p);
      case "pong":
        return this.onPong(p.seq, p.ts);
    }
  }

  private onHello(m: ServerMsg) {
    this.attempt = 0;
    // Trade ids are only ordered within one backend process. A restarted
    // backend (e.g. a free host waking from sleep) starts again at 1, so
    // dedupe state from the previous connection must not carry over.
    this.lastTradeId = 0;
    this.ltpSeq = 0;
    if (typeof m.tickSize === "number" && m.tickSize > 0) this.tick = m.tickSize;
    this.log(`connected as ${m.connId}`, "info");
    this.queue({
      conn: { status: "live", connId: String(m.connId), disconnectedAt: null, reconnectAt: null, attempt: 0 },
      tier: { table: m.tiers as Record<TierName, Rates> },
    });
    if (m.clock) useMarket.setState({ clock: m.clock as MarketState["clock"] });
    // Fast start: 3 probes 200 ms apart fill the server's warmup window (3
    // samples), so the first tier is decided in ~0.6 s; then 1 probe/s.
    this.probeSeq = 0;
    this.ping();
    for (const at of FAST_START_MS) this.retry(() => this.ping(), at);
    this.pingTimer = setInterval(() => this.ping(), PING_EVERY_MS);
    this.depth.start("initial");
    this.subscribeChart("initial");
    this.fetchTicker();
    if (this.simLatencyMs > 0) this.setSimLatency(this.simLatencyMs); // re-apply per connection
  }

  private onTier(m: ServerMsg) {
    const tier = m.tier as TierName;
    if (m.changed) this.log(`tier -> ${tier} (${m.reason})`, tier === "FULL" ? "info" : "warn");
    this.queue({
      tier: {
        tier,
        autoTier: m.autoTier as TierName,
        override: m.override as Override,
        reason: String(m.reason ?? ""),
        rates: m.rates as Rates,
        serverEffectiveMs: Number(m.effectiveLatencyMs ?? 0),
        latencyMs: Number(m.latencyMs ?? 0),
        jitterMs: Number(m.jitterMs ?? 0),
        warmedUp: Boolean(m.warmedUp),
      },
    });
  }

  private onTrades(p: TradeUpdate) {
    if (p.newestId <= this.lastTradeId) return; // duplicate or stale
    this.lastTradeId = p.newestId;
    this.queue({ trades: { list: p.trades, newestId: p.newestId, updatedAt: Date.now() } });
    this.updateLtp(p.newestId, p.trades[0].price);
  }

  /** LTP comes from whichever stream carries the newest trade id. */
  private updateLtp(seq: number, price: number) {
    if (seq <= this.ltpSeq) return;
    const dir = this.ltp === null || price === this.ltp ? 0 : price > this.ltp ? 1 : -1;
    this.ltpSeq = seq;
    this.ltp = price;
    this.queue({ ticker: dir === 0 ? { ltp: price, ltpSeq: seq } : { ltp: price, ltpSeq: seq, dir } });
  }

  // ---------------------------------------------------------------- latency

  private ping() {
    this.send(encodePing(++this.probeSeq, performance.now() * 1000));
  }

  private onPong(seq: number, tsMicros: number) {
    const rtt = (performance.now() * 1000 - tsMicros) / 1000;
    if (seq > this.probeSeq || rtt < 0 || rtt > 60_000) {
      this.log("invalid pong ignored", "warn");
      return;
    }
    if (document.hidden) return; // throttled timers would inflate RTT
    this.meter.add(rtt);
    const { latency, jitter, score, samples } = this.meter;
    this.send({ type: "NET_REPORT", latencyMs: latency, jitterMs: jitter, rttMs: rtt, samples });
    this.queue({ net: { rttMs: rtt, latencyMs: latency, jitterMs: jitter, scoreMs: score, samples } });
  }

  private publishRates() {
    const now = performance.now();
    this.queue({
      measured: { chart: this.rates.chart.rate(now), depth: this.rates.depth.rate(now), trades: this.rates.trades.rate(now) },
    });
  }

  // ---------------------------------------------------------------- REST

  private retry(fn: () => void, ms: number) {
    const t = setTimeout(() => {
      this.retryTimers.delete(t);
      fn();
    }, ms);
    this.retryTimers.add(t);
  }

  private fetchBook(token: number) {
    this.bookAbort?.abort();
    const ctl = (this.bookAbort = new AbortController());
    api
      .book(ctl.signal)
      .then((s: BookSnapshot) => this.depth.onSnapshot({ seq: s.seq, bids: s.bids, asks: s.asks }, token))
      .catch((e: Error) => {
        if (ctl.signal.aborted) return;
        this.log(`book snapshot failed: ${e.message}`, "warn");
        this.retry(() => {
          if (this.depth.currentToken === token && this.ws) this.depth.start("snapshot retry");
        }, 1000);
      });
  }

  private fetchCandles(token: number) {
    const interval = this.chartInterval;
    this.chartAbort?.abort();
    const ctl = (this.chartAbort = new AbortController());
    this.queue({ chart: { loading: true, error: null } });
    api
      .candles(interval, HISTORY_LIMIT, ctl.signal)
      .then((r: CandlesResponse) => {
        // A response for an interval the user has since switched away from is discarded.
        if (r.interval !== this.chartInterval || interval !== this.chartInterval) return;
        const state: ChartState = { seq: r.seq, interval, closed: normalizeCandles(r.candles ?? []), active: r.active };
        this.chart.onSnapshot(state, token);
        this.queue({ chart: { resetToken: ++this.chartResetToken, loading: false, error: null, interval } });
      })
      .catch((e: Error) => {
        if (ctl.signal.aborted) return;
        this.log(`candles request failed: ${e.message}`, "warn");
        this.queue({ chart: { loading: false, error: e.message } });
        this.retry(() => {
          if (this.chart.currentToken === token && this.ws) this.chart.start("snapshot retry");
        }, 1500);
      });
  }

  private fetchTicker() {
    this.tickerAbort?.abort();
    const ctl = (this.tickerAbort = new AbortController());
    api
      .ticker(ctl.signal)
      .then((t) => {
        this.queue({ ticker: { open24h: t.open24h, high24h: t.high24h, low24h: t.low24h, volume24h: t.volume24h } });
        if (this.ltp === null) this.queue({ ticker: { ltp: t.ltp } });
      })
      .catch(() => {});
  }

  // ---------------------------------------------------------------- controls

  private subscribeChart(reason: string) {
    this.awaitingSubAck = true;
    this.send({ type: "SUBSCRIBE", symbol: "BTCUSDT", interval: this.chartInterval });
    this.chart.start(reason);
  }

  setChartInterval(interval: string) {
    if (interval === this.chartInterval) return;
    this.chartInterval = interval;
    this.log(`interval -> ${interval}`, "info");
    this.queue({ chart: { interval, closed: [], active: null, seq: 0, loading: true, error: null, resetToken: ++this.chartResetToken } });
    if (this.ws?.readyState === WebSocket.OPEN) this.subscribeChart(`interval ${interval}`);
  }

  setOverride(tier: Override) {
    this.send({ type: "SET_TIER_OVERRIDE", tier });
  }

  private simLatencyMs = 0;
  setSimLatency(ms: number) {
    this.simLatencyMs = ms;
    this.queue({ debug: { simLatencyMs: ms } });
    this.send({ type: "DEBUG", action: "SIM_LATENCY", ms });
  }

  /** Delay exactly one PONG by 900 ms: a single outlier the tier must ignore. */
  spike() {
    this.send({ type: "DEBUG", action: "SPIKE" });
  }

  dropPacket(stream: "depth" | "chart") {
    this.send({ type: "DEBUG", action: stream === "depth" ? "DROP_DEPTH" : "DROP_CHART" });
  }

  /** Drop the socket; normal auto-reconnect follows. */
  kill() {
    this.log("debug: socket closed by user", "warn");
    this.ws?.close();
  }

  /** Stay offline until resumed (to demonstrate stale state). */
  setPaused(paused: boolean) {
    this.paused = paused;
    this.queue({ conn: { paused } });
    if (paused) {
      if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
      this.ws?.close();
      this.queue({ conn: { status: "offline", reconnectAt: null } });
    } else {
      this.attempt = 0;
      this.connect();
    }
  }

  // ---------------------------------------------------------------- batching

  private log(msg: string, level: "info" | "warn") {
    this.events = [{ id: ++this.eventId, at: Date.now(), level, msg }, ...this.events].slice(0, 60);
    this.eventsDirty = true;
    this.scheduleFlush();
  }

  private queue(patch: Patch) {
    for (const k of Object.keys(patch) as (keyof Patch)[]) {
      this.pending[k] = { ...(this.pending[k] ?? {}), ...patch[k] } as never;
    }
    this.scheduleFlush();
  }

  private scheduleFlush() {
    if (this.frame !== null || this.disposed) return;
    this.frame = requestAnimationFrame(() => {
      this.frame = null;
      const patch = this.pending;
      this.pending = {};
      commit(patch, this.eventsDirty ? this.events : undefined);
      this.eventsDirty = false;
    });
  }
}
