import type { ChartDelta } from "../protocol";
import { SyncError } from "./streamSync";

export interface Candle {
  t: number; // start, unix ms
  o: number;
  h: number;
  l: number;
  c: number;
  v: number;
}

export interface ChartState {
  seq: number;
  interval: string;
  closed: Candle[]; // oldest first, unique by t
  active: Candle | null;
}

/** Sort by time and drop duplicate candles (last one wins). */
export function normalizeCandles(list: Candle[]): Candle[] {
  const byTime = new Map<number, Candle>();
  for (const c of list) {
    if (Number.isFinite(c.t) && c.t > 0) byTime.set(c.t, c);
  }
  return [...byTime.values()].sort((a, b) => a.t - b.t);
}

function pushClosed(closed: Candle[], c: Candle): Candle[] {
  const last = closed[closed.length - 1];
  if (last && last.t === c.t) return [...closed.slice(0, -1), c]; // duplicate candle: replace
  if (last && last.t > c.t) return normalizeCandles([...closed, c]);
  const next = [...closed, c];
  return next.length > MAX_CANDLES ? next.slice(-MAX_CANDLES) : next;
}

const MAX_CANDLES = 5000;

/**
 * Apply one CHART_DELTA. The header timestamp identifies the candle:
 *  - same start as our active candle: add the deltas
 *  - newer start: finalise our active candle and open a new one whose values
 *    are the deltas from zero (never applied on top of the previous candle)
 *  - older start: impossible on an ordered stream -> resync
 */
export function applyChartDelta(s: ChartState, d: ChartDelta): ChartState {
  if (d.baseSeq !== s.seq) throw new SyncError(`base ${d.baseSeq} != local ${s.seq}`);
  const [o, h, l, c, v] = d.delta;
  const a = s.active;
  if (a && d.start === a.t) {
    return { ...s, seq: d.seq, active: { t: a.t, o: a.o + o, h: a.h + h, l: a.l + l, c: a.c + c, v: a.v + v } };
  }
  if (!a || d.start > a.t) {
    const closed = a ? pushClosed(s.closed, a) : s.closed;
    const next = { t: d.start, o, h, l, c, v };
    if (next.l > next.h || next.v < 0) throw new SyncError("invalid new candle");
    return { ...s, seq: d.seq, closed, active: next };
  }
  throw new SyncError(`delta for older candle ${d.start} < ${a.t}`);
}
