"use client";

import Link from "next/link";
import { fmtPrice, fmtQty, fmtTime } from "@/lib/fixed";
import { useMarket } from "@/store/market";

export function RecentTrades() {
  const trades = useMarket((s) => s.trades);
  const live = useMarket((s) => s.conn.status === "live");

  return (
    <section className="panel flex flex-col">
      <div className="flex items-center justify-between border-b border-line px-3 py-2">
        <h2 className="text-sm font-medium">Recent trades</h2>
        <div className="flex items-center gap-3 text-xs">
          {!live && <span className="rounded bg-warn/15 px-2 py-0.5 text-warn">STALE</span>}
          <Link href="/trades" className="text-info hover:underline">
            History →
          </Link>
        </div>
      </div>
      <div className={live ? "" : "stale"}>
        <div className="grid grid-cols-[1fr_1fr_1fr_auto] gap-2 px-3 py-1.5 text-[11px] text-muted">
          <span>Price</span>
          <span className="text-right">Size (BTC)</span>
          <span className="text-right">Time</span>
          <span className="w-12 text-right">ID</span>
        </div>
        {trades.list.length === 0 && <div className="p-6 text-center text-sm text-muted">Waiting for trades…</div>}
        {trades.list.map((t) => (
          <div key={t.id} className="num grid grid-cols-[1fr_1fr_1fr_auto] gap-2 px-3 py-[3px] text-xs">
            <span className={t.side > 0 ? "text-up" : "text-down"}>{fmtPrice(t.price)}</span>
            <span className="text-right">{fmtQty(t.qty)}</span>
            <span className="text-right text-muted">{fmtTime(t.ts, true)}</span>
            <span className="w-12 text-right text-muted">{t.id}</span>
          </div>
        ))}
      </div>
    </section>
  );
}
