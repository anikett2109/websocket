import { LEVELS, type DepthDelta } from "../protocol";
import { SyncError } from "./streamSync";

export interface Level {
  price: number;
  qty: number;
}

export interface BookState {
  seq: number;
  bids: Level[]; // best first
  asks: Level[]; // best first
}

const qtyAt = (levels: Level[], price: number) => levels.find((l) => l.price === price)?.qty ?? 0;

/**
 * Apply one DEPTH_DELTA. Level prices come from the packet's anchors
 * (bid[i] = bestBid - i*tick, ask[i] = bestAsk + i*tick); each quantity delta
 * is relative to our quantity at that same price (0 if we had none), so a
 * shifted book re-indexes naturally. Zero-quantity levels are removed.
 */
export function applyDepthDelta(book: BookState, d: DepthDelta, tick: number): BookState {
  if (d.baseSeq !== book.seq) throw new SyncError(`base ${d.baseSeq} != local ${book.seq}`);
  if (d.bestBid >= d.bestAsk) throw new SyncError("crossed book in delta");
  const bids: Level[] = [];
  const asks: Level[] = [];
  for (let i = 0; i < LEVELS; i++) {
    const bp = d.bestBid - i * tick;
    const ap = d.bestAsk + i * tick;
    const bq = qtyAt(book.bids, bp) + d.bid[i];
    const aq = qtyAt(book.asks, ap) + d.ask[i];
    if (bq < 0 || aq < 0) throw new SyncError(`negative quantity at level ${i}`);
    if (bq > 0) bids.push({ price: bp, qty: bq });
    if (aq > 0) asks.push({ price: ap, qty: aq });
  }
  return { seq: d.seq, bids, asks };
}
