"use client";

import { memo } from "react";
import { fmtPrice, fmtQty } from "@/lib/fixed";
import type { Level } from "@/lib/sync/book";
import { useMarket } from "@/store/market";

export function OrderBook() {
  const book = useMarket((s) => s.book);
  const live = book.phase === "live";
  const asks = book.asks.slice(0, 10);
  const bids = book.bids.slice(0, 10);

  // Cumulative depth for the background bars.
  const cum = (ls: Level[]) => ls.reduce<number[]>((acc, l, i) => (acc.push((acc[i - 1] ?? 0) + l.qty), acc), []);
  const askCum = cum(asks);
  const bidCum = cum(bids);
  const maxCum = Math.max(askCum[askCum.length - 1] ?? 0, bidCum[bidCum.length - 1] ?? 0, 1);
  const spread = asks[0] && bids[0] ? asks[0].price - bids[0].price : null;

  return (
    <section className="panel flex flex-col">
      <div className="flex items-center justify-between border-b border-line px-3 py-2">
        <h2 className="text-sm font-medium">Order book</h2>
        <span className="num text-xs text-muted">
          {!live && <span className="mr-2 rounded bg-warn/15 px-2 py-0.5 text-warn">{book.phase === "idle" ? "STALE" : "SYNCING"}</span>}
          seq {book.seq}
        </span>
      </div>
      <div className={live ? "" : "stale"}>
        <Header />
        <div className="flex flex-col-reverse">
          {asks.map((l, i) => (
            <Row key={i} level={l} cum={askCum[i]} max={maxCum} side="ask" />
          ))}
        </div>
        <div className="num flex items-center justify-between border-y border-line bg-panel-2 px-3 py-1.5 text-xs">
          <span className="text-muted">Spread</span>
          <span>{spread !== null ? fmtPrice(spread) : "—"}</span>
        </div>
        <div>
          {bids.map((l, i) => (
            <Row key={i} level={l} cum={bidCum[i]} max={maxCum} side="bid" />
          ))}
        </div>
        {asks.length === 0 && bids.length === 0 && <div className="p-6 text-center text-sm text-muted">Waiting for snapshot…</div>}
      </div>
    </section>
  );
}

function Header() {
  return (
    <div className="grid grid-cols-3 px-3 py-1.5 text-[11px] text-muted">
      <span>Price (USDT)</span>
      <span className="text-right">Size (BTC)</span>
      <span className="text-right">Total</span>
    </div>
  );
}

const Row = memo(function Row({ level, cum, max, side }: { level: Level; cum: number; max: number; side: "bid" | "ask" }) {
  const pct = Math.min(100, (cum / max) * 100);
  return (
    <div className="num relative grid grid-cols-3 px-3 py-[3px] text-xs">
      <div
        className="absolute inset-y-0 right-0"
        style={{ width: `${pct}%`, background: side === "bid" ? "var(--up-soft)" : "var(--down-soft)" }}
      />
      <span className={`relative ${side === "bid" ? "text-up" : "text-down"}`}>{fmtPrice(level.price)}</span>
      <span className="relative text-right">{fmtQty(level.qty)}</span>
      <span className="relative text-right text-muted">{fmtQty(cum, 3)}</span>
    </div>
  );
});
