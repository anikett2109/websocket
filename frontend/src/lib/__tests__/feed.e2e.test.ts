// End-to-end: runs the real FeedClient in Node against a running backend.
// Opt-in:  E2E_API_URL=http://localhost:8080 npm test
//
// It proves the core guarantees on the live system: the locally synchronised
// book and candles are identical to the server's canonical state at the same
// sequence, at FULL and at a forced MINIMAL tier, after an injected gap, after
// an interval switch and after a reconnect.
import { afterAll, beforeAll, describe, expect, it } from "vitest";

const API = process.env.E2E_API_URL;
const run = API ? describe : describe.skip;

type Any = Record<string, unknown>;

run("FeedClient against a live backend", () => {
  // Minimal browser surface FeedClient needs; Node 22 provides WebSocket and fetch.
  const g = globalThis as Any;
  g.document = { visibilityState: "visible", hidden: false, addEventListener() {}, removeEventListener() {} };
  g.window = { addEventListener() {}, removeEventListener() {} };
  g.requestAnimationFrame = (cb: () => void) => setTimeout(cb, 16);
  g.cancelAnimationFrame = (t: ReturnType<typeof setTimeout>) => clearTimeout(t);
  process.env.NEXT_PUBLIC_API_URL = API;

  /* eslint-disable @typescript-eslint/no-explicit-any */
  let client: any;
  let store: any;
  let api: any;
  const books = new Map<number, string>();
  const charts = new Map<number, string>();
  let recorder: ReturnType<typeof setInterval>;

  const wait = async (what: string, cond: () => boolean, ms = 15_000) => {
    const end = Date.now() + ms;
    while (!cond()) {
      if (Date.now() > end) throw new Error(`timeout waiting for ${what}`);
      await new Promise((r) => setTimeout(r, 20));
    }
  };
  const s = () => store.useMarket.getState();

  beforeAll(async () => {
    ({ api } = await import("../api"));
    store = await import("@/store/market");
    const { FeedClient } = await import("../feed");
    const { WS_URL } = await import("../api");
    client = new FeedClient(WS_URL);
    client.start();
    // Record every local state by seq so it can be compared to REST snapshots.
    recorder = setInterval(() => {
      const b = client.depth.state;
      if (b && client.depth.phase === "live") books.set(b.seq, JSON.stringify([b.bids, b.asks]));
      const c = client.chart.state;
      if (c && client.chart.phase === "live") charts.set(c.seq, JSON.stringify([c.interval, c.active, c.closed.slice(-3)]));
    }, 2);
    await wait("live", () => s().conn.status === "live" && client.depth.phase === "live" && client.chart.phase === "live");
  }, 30_000);

  afterAll(() => {
    clearInterval(recorder);
    client?.dispose();
  });

  /** Poll REST until a snapshot's seq matches a recorded local state, then compare. */
  async function expectBookMatchesServer() {
    books.clear();
    for (let i = 0; i < 400; i++) {
      const snap = await api.book();
      const local = books.get(snap.seq);
      if (local) {
        expect(local).toBe(JSON.stringify([snap.bids, snap.asks]));
        return snap.seq;
      }
      await new Promise((r) => setTimeout(r, 7));
    }
    throw new Error("never observed a REST book snapshot at a locally held seq");
  }

  async function expectChartMatchesServer(interval: string) {
    charts.clear();
    for (let i = 0; i < 400; i++) {
      const r = await api.candles(interval);
      const local = charts.get(r.seq);
      if (local) {
        expect(local).toBe(JSON.stringify([interval, r.active, r.candles.slice(-3)]));
        return r.seq;
      }
      await new Promise((r2) => setTimeout(r2, 7));
    }
    throw new Error("never observed REST candles at a locally held seq");
  }

  it("local book and candles equal the server's canonical state (FULL/auto)", async () => {
    await expectBookMatchesServer();
    await expectChartMatchesServer("1m");
  }, 30_000);

  it("forced MINIMAL tier: slower delivery, identical data", async () => {
    client.setOverride("MINIMAL");
    await wait("MINIMAL", () => s().tier.tier === "MINIMAL");
    await new Promise((r) => setTimeout(r, 6000));
    expect(s().measured.chart).toBeLessThanOrEqual(1.4); // target 1/s
    expect(s().measured.depth).toBeLessThanOrEqual(4.6); // target 4/s
    await expectBookMatchesServer();
    await expectChartMatchesServer("1m");
    client.setOverride("AUTO");
    await wait("AUTO", () => s().tier.override === "AUTO");
  }, 60_000);

  it("injected depth gap is detected and recovered", async () => {
    const before = s().events.length;
    client.dropPacket("depth");
    await wait("gap event", () => s().events.slice(0, s().events.length - before + 5).some((e: Any) => String(e.msg).includes("gap detected")));
    await wait("book live again", () => client.depth.phase === "live");
    await expectBookMatchesServer();
  }, 30_000);

  it("interval switch discards the old stream and syncs the new one", async () => {
    client.setChartInterval("5m");
    await wait("5m live", () => client.chart.phase === "live" && client.chart.state?.interval === "5m");
    await expectChartMatchesServer("5m");
  }, 30_000);

  it("reconnects after the socket is killed and resynchronises", async () => {
    client.kill();
    await wait("disconnect", () => s().conn.status !== "live");
    await wait("reconnected", () => s().conn.status === "live" && client.depth.phase === "live" && client.chart.phase === "live", 20_000);
    await expectBookMatchesServer();
    await expectChartMatchesServer("5m");
  }, 40_000);

  it("a single latency spike does not change the tier (robust median)", async () => {
    await wait("FULL on localhost", () => s().tier.autoTier === "FULL", 20_000);
    const changes = s().events.filter((e: Any) => String(e.msg).startsWith("tier ->")).length;
    client.spike();
    await new Promise((r) => setTimeout(r, 6000));
    expect(s().tier.autoTier).toBe("FULL");
    expect(s().events.filter((e: Any) => String(e.msg).startsWith("tier ->")).length).toBe(changes);
  }, 40_000);

  it("automatic tiering: +700 ms demotes to MINIMAL, removing it promotes", async () => {
    client.setSimLatency(700); // L ≈ 700 ≥ 600
    await wait("auto MINIMAL", () => s().tier.autoTier === "MINIMAL", 25_000);
    client.setSimLatency(0);
    await wait("promotion to DEGRADED", () => s().tier.autoTier === "DEGRADED", 20_000);
    await wait("promotion to FULL", () => s().tier.autoTier === "FULL", 20_000);
  }, 90_000);
});
