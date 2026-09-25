"use client";

import { useEffect, useRef, useState } from "react";
import {
  CandlestickSeries,
  ColorType,
  createChart,
  CrosshairMode,
  HistogramSeries,
  type IChartApi,
  type ISeriesApi,
  type MouseEventParams,
  type Time,
  type UTCTimestamp,
} from "lightweight-charts";
import { fmtChangePct, fmtDateTime, fmtPrice, fmtQty, toChartPrice, toChartQty } from "@/lib/fixed";
import type { Candle } from "@/lib/sync/chart";
import { useMarket, type MarketState } from "@/store/market";
import { useFeed } from "./FeedProvider";

const INTERVALS = ["1m", "5m"] as const;
const UP = "#1fb87a";
const DOWN = "#f0495c";

// lightweight-charts renders UTC; shift by the local offset so the axis shows local time.
const tzShift = () => -new Date().getTimezoneOffset() * 60;
const toTime = (t: number) => (t / 1000 + tzShift()) as UTCTimestamp;
const fromTime = (t: Time) => ((t as number) - tzShift()) * 1000;

const bar = (c: Candle) => ({
  time: toTime(c.t),
  open: toChartPrice(c.o),
  high: toChartPrice(c.h),
  low: toChartPrice(c.l),
  close: toChartPrice(c.c),
});
const vol = (c: Candle) => ({ time: toTime(c.t), value: toChartQty(c.v), color: c.c >= c.o ? "rgba(31,184,122,.35)" : "rgba(240,73,92,.35)" });

export function CandleChart() {
  const feed = useFeed();
  const interval = useMarket((s) => s.chart.interval);
  const phase = useMarket((s) => s.chart.phase);
  const loading = useMarket((s) => s.chart.loading);
  const error = useMarket((s) => s.chart.error);
  const empty = useMarket((s) => !s.chart.loading && s.chart.closed.length === 0 && !s.chart.active);
  const live = phase === "live";

  const box = useRef<HTMLDivElement>(null);
  const byTime = useRef(new Map<number, Candle>());
  const [hover, setHover] = useState<Candle | null>(null);
  const [pinned, setPinned] = useState<Candle | null>(null);

  useEffect(() => {
    const el = box.current!;
    const chart: IChartApi = createChart(el, {
      autoSize: true,
      layout: { background: { type: ColorType.Solid, color: "transparent" }, textColor: "#8a93a3", fontSize: 11 },
      grid: { vertLines: { color: "rgba(35,42,54,.6)" }, horzLines: { color: "rgba(35,42,54,.6)" } },
      crosshair: { mode: CrosshairMode.Normal },
      rightPriceScale: { borderColor: "#232a36" },
      timeScale: { borderColor: "#232a36", timeVisible: true, secondsVisible: false, rightOffset: 4 },
    });
    const candles: ISeriesApi<"Candlestick"> = chart.addSeries(CandlestickSeries, {
      upColor: UP, downColor: DOWN, borderVisible: false, wickUpColor: UP, wickDownColor: DOWN,
    });
    const volume: ISeriesApi<"Histogram"> = chart.addSeries(HistogramSeries, { priceScaleId: "vol", priceFormat: { type: "volume" } });
    chart.priceScale("vol").applyOptions({ scaleMargins: { top: 0.82, bottom: 0 } });

    const lookup = (p: MouseEventParams) => (p.time === undefined ? null : byTime.current.get(fromTime(p.time)) ?? null);
    const onMove = (p: MouseEventParams) => setHover(lookup(p));
    const onClick = (p: MouseEventParams) => setPinned((cur) => {
      const c = lookup(p);
      return c && cur?.t !== c.t ? c : null; // click a candle to pin, click again to unpin
    });
    chart.subscribeCrosshairMove(onMove);
    chart.subscribeClick(onClick);

    // Apply store changes imperatively: the chart never re-renders through React.
    let resetToken = -1;
    let lastT = 0;
    const render = (c: MarketState["chart"]) => {
      if (c.resetToken !== resetToken) {
        resetToken = c.resetToken;
        const all = c.active ? [...c.closed, c.active] : c.closed;
        byTime.current = new Map(all.map((x) => [x.t, x]));
        candles.setData(all.map(bar));
        volume.setData(all.map(vol));
        lastT = all.length ? all[all.length - 1].t : 0;
        if (all.length) chart.timeScale().setVisibleLogicalRange({ from: all.length - 120, to: all.length + 4 });
        setPinned(null);
        return;
      }
      // Incremental: newly closed candles (finalised values), then the active one.
      const fresh = c.closed.filter((x) => x.t >= lastT);
      if (c.active) fresh.push(c.active);
      for (const x of fresh) {
        if (x.t < lastT) continue;
        byTime.current.set(x.t, x);
        candles.update(bar(x));
        volume.update(vol(x));
        lastT = x.t;
      }
    };
    render(useMarket.getState().chart);
    const unsub = useMarket.subscribe((s, prev) => {
      if (s.chart !== prev.chart) render(s.chart);
    });

    return () => {
      unsub();
      chart.unsubscribeCrosshairMove(onMove);
      chart.unsubscribeClick(onClick);
      chart.remove();
    };
  }, []);

  const shown = pinned ?? hover;

  return (
    <section className="panel flex h-[460px] flex-col lg:h-auto lg:min-h-[600px] lg:flex-1">
      <div className="flex flex-wrap items-center gap-3 border-b border-line px-3 py-2">
        <div className="flex rounded-md bg-panel-2 p-0.5" role="tablist" aria-label="Candle interval">
          {INTERVALS.map((i) => (
            <button
              key={i}
              role="tab"
              aria-selected={interval === i}
              onClick={() => feed?.setChartInterval(i)}
              className={`rounded px-3 py-1 text-xs font-medium ${interval === i ? "bg-line text-text" : "text-muted hover:text-text"}`}
            >
              {i}
            </button>
          ))}
        </div>
        <Legend c={shown} pinned={!!pinned} />
        <div className="ml-auto flex items-center gap-2 text-xs">
          {!live && <span className="rounded bg-warn/15 px-2 py-0.5 text-warn">{phase === "idle" ? "STALE" : "SYNCING"}</span>}
        </div>
      </div>
      <div className={`relative flex-1 ${live || loading ? "" : "stale"}`}>
        <div ref={box} className="absolute inset-0" />
        {loading && <Overlay>Loading {interval} history…</Overlay>}
        {!loading && error && <Overlay>Could not load candles: {error}. Retrying…</Overlay>}
        {!loading && !error && empty && <Overlay>No candles yet for {interval}; waiting for the first trade.</Overlay>}
      </div>
    </section>
  );
}

function Overlay({ children }: { children: React.ReactNode }) {
  return <div className="pointer-events-none absolute inset-0 grid place-items-center text-sm text-muted">{children}</div>;
}

function Legend({ c, pinned }: { c: Candle | null; pinned: boolean }) {
  if (!c) return <span className="text-xs text-muted">Hover a candle to inspect · click to pin · drag to pan</span>;
  const up = c.c >= c.o;
  const cls = up ? "text-up" : "text-down";
  return (
    <div className="num flex flex-wrap items-center gap-x-3 text-xs">
      <span className="text-muted">{fmtDateTime(c.t)}{pinned ? " · pinned" : ""}</span>
      <span>O <b className={cls}>{fmtPrice(c.o)}</b></span>
      <span>H <b className={cls}>{fmtPrice(c.h)}</b></span>
      <span>L <b className={cls}>{fmtPrice(c.l)}</b></span>
      <span>C <b className={cls}>{fmtPrice(c.c)}</b></span>
      <span>V <b>{fmtQty(c.v, 3)}</b></span>
      <span className={cls}>{fmtChangePct(c.o, c.c)}</span>
    </div>
  );
}
