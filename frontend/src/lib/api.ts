// REST client. Base URLs come from build-time env so the same code works
// locally and against a separately deployed backend.
import type { Candle } from "./sync/chart";
import type { Level } from "./sync/book";

export const API_URL = (process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080").replace(/\/$/, "");
export const WS_URL = process.env.NEXT_PUBLIC_WS_URL ?? API_URL.replace(/^http/, "ws") + "/ws";
export const SYMBOL = "BTCUSDT";
// The backend keeps 3 days of history (4320 x 1m, 864 x 5m); request all of it.
export const HISTORY_LIMIT = 5000;

export class ApiError extends Error {}

async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(API_URL + path, { signal, cache: "no-store" });
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`;
    try {
      msg = ((await res.json()) as { error?: string }).error ?? msg;
    } catch {}
    throw new ApiError(msg);
  }
  return (await res.json()) as T;
}

export interface CandlesResponse {
  symbol: string;
  interval: string;
  seq: number;
  ltp: number;
  candles: Candle[];
  active: Candle | null;
}

export interface BookSnapshot {
  symbol: string;
  seq: number;
  ltp: number;
  ltq: number;
  tickSize: number;
  bids: Level[];
  asks: Level[];
}

export interface TickerResponse {
  ltp: number;
  open24h: number;
  high24h: number;
  low24h: number;
  volume24h: number;
}

export interface RestTrade {
  id: number;
  ts: number;
  price: number;
  qty: number;
  side: 1 | -1;
}

export const api = {
  candles: (interval: string, limit = HISTORY_LIMIT, signal?: AbortSignal) =>
    get<CandlesResponse>(`/api/candles?symbol=${SYMBOL}&interval=${interval}&limit=${limit}`, signal),
  book: (signal?: AbortSignal) => get<BookSnapshot>(`/api/orderbook/snapshot?symbol=${SYMBOL}`, signal),
  ticker: (signal?: AbortSignal) => get<TickerResponse>(`/api/ticker?symbol=${SYMBOL}`, signal),
  trades: (from: number, to: number, limit: number, signal?: AbortSignal) =>
    get<{ trades: RestTrade[]; retainedFrom: number }>(
      `/api/trades?symbol=${SYMBOL}&from=${from}&to=${to}&limit=${limit}`,
      signal,
    ),
};
