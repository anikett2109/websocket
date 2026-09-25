"use client";

import Link from "next/link";
import { useCallback, useEffect, useRef, useState } from "react";
import { api, type RestTrade } from "@/lib/api";
import { fmtDateTime, fmtPrice, fmtQty, fmtTime } from "@/lib/fixed";
import { ConnectionBadge } from "./TopBar";

const PRESETS = [
  { label: "Last 1 min", ms: 60_000 },
  { label: "Last 5 min", ms: 300_000 },
  { label: "Last 15 min", ms: 900_000 },
];

// <input type="datetime-local"> works in local time without a zone suffix.
const toLocalInput = (ms: number) => {
  const d = new Date(ms - new Date(ms).getTimezoneOffset() * 60_000);
  return d.toISOString().slice(0, 19);
};
const fromLocalInput = (s: string) => (s ? new Date(s).getTime() : 0);

/** Arbitrary time-range trade history, served by REST from the server's ring buffer. */
export function TradeHistory() {
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [limit, setLimit] = useState(200);
  const [rows, setRows] = useState<RestTrade[]>([]);
  const [retainedFrom, setRetainedFrom] = useState(0);
  const [status, setStatus] = useState<"idle" | "loading" | "error">("loading");
  const [error, setError] = useState("");
  const ctl = useRef<AbortController | null>(null);

  // Only sets state from promise callbacks, so it is safe to call from an effect.
  const fetchRange = useCallback((fromMs: number, toMs: number, lim: number) => {
    ctl.current?.abort(); // a newer query supersedes any in-flight one
    const c = (ctl.current = new AbortController());
    api
      .trades(fromMs, toMs, lim, c.signal)
      .then((r) => {
        setRows(r.trades);
        setRetainedFrom(r.retainedFrom);
        setStatus("idle");
      })
      .catch((e: Error) => {
        if (c.signal.aborted) return;
        setError(e.message);
        setStatus("error");
      });
  }, []);

  const load = useCallback(
    (fromMs: number, toMs: number, lim: number) => {
      setStatus("loading");
      fetchRange(fromMs, toMs, lim);
    },
    [fetchRange],
  );

  const applyPreset = useCallback(
    (ms: number) => {
      const now = Date.now();
      setFrom(toLocalInput(now - ms));
      setTo(toLocalInput(now));
      load(now - ms, now, limit);
    },
    [limit, load],
  );

  useEffect(() => {
    const now = Date.now();
    fetchRange(now - 60_000, now, 200); // initial query: last minute
    return () => ctl.current?.abort();
  }, [fetchRange]);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const f = fromLocalInput(from);
    const t = fromLocalInput(to);
    if (f && t && f > t) {
      setError("'From' must be before 'To'");
      setStatus("error");
      return;
    }
    load(f, t, limit);
  };

  const input = "rounded-md border border-line bg-panel-2 px-2 py-1 text-xs text-text";

  return (
    <>
      <header className="panel flex flex-wrap items-center gap-3 px-4 py-3">
        <Link href="/" className="text-sm text-info hover:underline">
          ← Trading screen
        </Link>
        <h1 className="font-semibold">BTC/USDT trade history</h1>
        <div className="ml-auto">
          <ConnectionBadge />
        </div>
      </header>

      <section className="panel p-3">
        <form onSubmit={submit} className="flex flex-wrap items-end gap-3 text-xs">
          <label className="flex flex-col gap-1">
            <span className="text-muted">From</span>
            <input type="datetime-local" step={1} value={from} onChange={(e) => setFrom(e.target.value)} className={input} />
          </label>
          <label className="flex flex-col gap-1">
            <span className="text-muted">To</span>
            <input type="datetime-local" step={1} value={to} onChange={(e) => setTo(e.target.value)} className={input} />
          </label>
          <label className="flex flex-col gap-1">
            <span className="text-muted">Limit</span>
            <select value={limit} onChange={(e) => setLimit(Number(e.target.value))} className={input}>
              {[50, 200, 1000, 5000].map((n) => (
                <option key={n} value={n}>{n}</option>
              ))}
            </select>
          </label>
          <button type="submit" className="rounded-md bg-accent px-3 py-1.5 font-medium text-black">
            Apply
          </button>
          <div className="flex gap-2">
            {PRESETS.map((p) => (
              <button type="button" key={p.label} onClick={() => applyPreset(p.ms)} className="rounded-md border border-line px-2 py-1.5 hover:bg-panel-2">
                {p.label}
              </button>
            ))}
          </div>
        </form>
        <p className="mt-2 text-[11px] text-muted">
          The server keeps a bounded in-memory buffer of recent trades
          {retainedFrom ? `; oldest retained trade: ${fmtDateTime(retainedFrom)}` : ""}. Newest first.
        </p>
      </section>

      <section className="panel">
        <div className="flex items-center justify-between border-b border-line px-3 py-2 text-xs">
          <span>{rows.length} trades</span>
          {status === "loading" && <span className="text-muted">Loading…</span>}
          {status === "error" && <span className="text-down">{error}</span>}
        </div>
        <div className="num grid grid-cols-[80px_1fr_1fr_1fr_60px] gap-2 px-3 py-1.5 text-[11px] text-muted">
          <span>ID</span>
          <span>Time</span>
          <span className="text-right">Price</span>
          <span className="text-right">Size (BTC)</span>
          <span className="text-right">Side</span>
        </div>
        <div className="max-h-[65vh] overflow-y-auto">
          {rows.length === 0 && status === "idle" && <div className="p-6 text-center text-sm text-muted">No trades in this range.</div>}
          {rows.map((t) => (
            <div key={t.id} className="num grid grid-cols-[80px_1fr_1fr_1fr_60px] gap-2 px-3 py-[3px] text-xs">
              <span className="text-muted">{t.id}</span>
              <span>{fmtTime(t.ts, true)}</span>
              <span className={`text-right ${t.side > 0 ? "text-up" : "text-down"}`}>{fmtPrice(t.price)}</span>
              <span className="text-right">{fmtQty(t.qty)}</span>
              <span className={`text-right ${t.side > 0 ? "text-up" : "text-down"}`}>{t.side > 0 ? "Buy" : "Sell"}</span>
            </div>
          ))}
        </div>
      </section>
    </>
  );
}
