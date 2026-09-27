"use client";

import { useEffect, useState } from "react";
import { fmtChangePct, fmtPrice, fmtQty } from "@/lib/fixed";
import { useMarket } from "@/store/market";
import { RegimeBadge } from "./RegimeBadge";

export function TopBar() {
  const t = useMarket((s) => s.ticker);
  const live = useMarket((s) => s.conn.status === "live");
  const change = t.ltp !== null && t.open24h ? t.ltp - t.open24h : 0;

  return (
    <header className="panel flex flex-wrap items-center gap-x-8 gap-y-3 px-4 py-3">
      <div className="flex items-center gap-3">
        <div className="grid h-8 w-8 place-items-center rounded-full bg-accent/15 font-bold text-accent">₿</div>
        <div>
          <div className="font-semibold leading-tight">BTC/USDT</div>
          <div className="text-xs text-muted">Simulated spot</div>
        </div>
      </div>

      <div className={live ? "" : "stale"}>
        <LastPrice ltp={t.ltp} dir={t.dir} />
      </div>

      <Stat label="24h change" className={change >= 0 ? "text-up" : "text-down"} stale={!live}>
        {t.ltp !== null && t.open24h ? `${change >= 0 ? "+" : ""}${fmtPrice(change)} ${fmtChangePct(t.open24h, t.ltp)}` : "—"}
      </Stat>
      <Stat label="24h high" stale={!live}>{t.high24h !== null ? fmtPrice(t.high24h) : "—"}</Stat>
      <Stat label="24h low" stale={!live}>{t.low24h !== null ? fmtPrice(t.low24h) : "—"}</Stat>
      <Stat label="24h volume (BTC)" stale={!live}>{t.volume24h !== null ? fmtQty(t.volume24h, 2) : "—"}</Stat>

      <div className="ml-auto flex items-center gap-6">
        <RegimeBadge />
        <ConnectionBadge />
      </div>
    </header>
  );
}

function LastPrice({ ltp, dir }: { ltp: number | null; dir: 1 | -1 | 0 }) {
  // Remount the span on each change so the flash animation restarts.
  return (
    <div className="flex items-baseline gap-2">
      <span
        key={ltp ?? 0}
        className={`num rounded px-1 text-2xl font-semibold ${dir > 0 ? "flash-up text-up" : dir < 0 ? "flash-down text-down" : ""}`}
      >
        {ltp !== null ? fmtPrice(ltp) : "—"}
      </span>
      <span className={`text-sm ${dir > 0 ? "text-up" : dir < 0 ? "text-down" : "text-muted"}`}>
        {dir > 0 ? "▲" : dir < 0 ? "▼" : ""}
      </span>
    </div>
  );
}

function Stat({ label, children, className = "", stale }: { label: string; children: React.ReactNode; className?: string; stale?: boolean }) {
  return (
    <div className={stale ? "stale" : ""}>
      <div className="text-xs text-muted">{label}</div>
      <div className={`num text-sm ${className}`}>{children}</div>
    </div>
  );
}

export function ConnectionBadge() {
  const conn = useMarket((s) => s.conn);
  const now = useNow(conn.status !== "live");

  const cfg = {
    live: { dot: "bg-up", label: "Live", text: "text-up" },
    connecting: { dot: "bg-info pulse", label: "Connecting", text: "text-info" },
    reconnecting: { dot: "bg-warn pulse", label: "Reconnecting", text: "text-warn" },
    offline: { dot: "bg-down", label: "Offline", text: "text-down" },
  }[conn.status];

  let detail = "";
  if (conn.status !== "live" && conn.disconnectedAt) {
    detail = `stale ${Math.max(0, Math.round((now - conn.disconnectedAt) / 1000))}s`;
    if (conn.reconnectAt) detail += ` · retry in ${Math.max(0, Math.ceil((conn.reconnectAt - now) / 1000))}s`;
  }

  return (
    <div className="flex items-center gap-2 rounded-full border border-line bg-panel-2 px-3 py-1.5 text-sm" role="status" aria-live="polite">
      <span className={`h-2 w-2 rounded-full ${cfg.dot}`} />
      <span className={cfg.text}>{cfg.label}</span>
      {detail && <span className="text-xs text-muted">{detail}</span>}
    </div>
  );
}

/** Re-render once per second while `active` (for countdowns). */
function useNow(active: boolean) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [active]);
  return now;
}
