// Application state (Zustand). The store is a passive sink: all networking and
// synchronisation logic lives in FeedClient, which commits batched changes here
// at most once per animation frame. Components subscribe to narrow slices with
// selectors so a depth update never re-renders the chart, and vice versa.
import { create } from "zustand";
import type { Trade } from "@/lib/protocol";
import type { Level } from "@/lib/sync/book";
import type { Candle } from "@/lib/sync/chart";
import type { Phase } from "@/lib/sync/streamSync";

export type ConnStatus = "connecting" | "live" | "reconnecting" | "offline";
export type TierName = "FULL" | "DEGRADED" | "MINIMAL";
export type Override = TierName | "AUTO";

export interface Rates {
  depthMs: number;
  chartMs: number;
  tradeMs: number;
}

export interface LogEvent {
  id: number;
  at: number;
  level: "info" | "warn";
  msg: string;
}

export interface MarketState {
  conn: {
    status: ConnStatus;
    connId: string | null;
    lastMessageAt: number;
    disconnectedAt: number | null;
    reconnectAt: number | null;
    attempt: number;
    paused: boolean;
  };
  tier: {
    tier: TierName | null;
    autoTier: TierName | null;
    override: Override;
    reason: string;
    rates: Rates | null;
    serverEffectiveMs: number; // L = median + 4·MAD of the last 5 RTTs (server)
    medianMs: number;
    madMs: number;
    warmedUp: boolean;
    table: Record<TierName, Rates> | null;
  };
  net: { rttMs: number; srttMs: number; rttvarMs: number; effectiveMs: number; samples: number };
  measured: { chart: number; depth: number; trades: number };
  ticker: { ltp: number | null; ltpSeq: number; dir: 1 | -1 | 0; open24h: number | null; high24h: number | null; low24h: number | null; volume24h: number | null };
  book: { bids: Level[]; asks: Level[]; seq: number; phase: Phase; updatedAt: number };
  chart: {
    interval: string;
    closed: Candle[];
    active: Candle | null;
    seq: number;
    phase: Phase;
    resetToken: number; // bumps when the whole series must be redrawn
    loading: boolean;
    error: string | null;
  };
  trades: { list: Trade[]; newestId: number; updatedAt: number };
  debug: { simLatencyMs: number };
  /** Market clock from HELLO: tick n = ⌊(t − epoch)/tickMs⌋, regime by n mod cycleTicks. */
  clock: { epochMs: number; tickMs: number; cycleTicks: number; normalEnd: number; burstEnd: number } | null;
  events: LogEvent[];
}

export const initialState: MarketState = {
  conn: { status: "connecting", connId: null, lastMessageAt: 0, disconnectedAt: null, reconnectAt: null, attempt: 0, paused: false },
  tier: { tier: null, autoTier: null, override: "AUTO", reason: "", rates: null, serverEffectiveMs: 0, medianMs: 0, madMs: 0, warmedUp: false, table: null },
  clock: null,
  net: { rttMs: 0, srttMs: 0, rttvarMs: 0, effectiveMs: 0, samples: 0 },
  measured: { chart: 0, depth: 0, trades: 0 },
  ticker: { ltp: null, ltpSeq: 0, dir: 0, open24h: null, high24h: null, low24h: null, volume24h: null },
  book: { bids: [], asks: [], seq: 0, phase: "idle", updatedAt: 0 },
  chart: { interval: "1m", closed: [], active: null, seq: 0, phase: "idle", resetToken: 0, loading: true, error: null },
  trades: { list: [], newestId: 0, updatedAt: 0 },
  debug: { simLatencyMs: 0 },
  events: [],
};

export const useMarket = create<MarketState>()(() => initialState);

type Slices = Omit<MarketState, "events">;
export type Patch = { [K in keyof Slices]?: Partial<Slices[K]> };

/** Shallow-merge each patched slice; used by FeedClient's frame-batched commit. */
export function commit(patch: Patch, events?: LogEvent[]) {
  useMarket.setState((s) => {
    const next: Record<string, unknown> = {};
    for (const k of Object.keys(patch) as (keyof Slices)[]) {
      next[k] = { ...s[k], ...patch[k] };
    }
    if (events) next.events = events;
    return next as Partial<MarketState>;
  });
}
