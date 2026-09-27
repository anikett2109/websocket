"use client";

import { useMarket } from "@/store/market";

const LABEL = {
  NORMAL: { rate: "20 trades/s", cls: "text-text" },
  BURST: { rate: "60 trades/s", cls: "text-warn" },
  QUIET: { rate: "1 trade/s", cls: "text-info" },
} as const;

/**
 * The deterministic market cycle (60 s: normal 30 s → burst 10 s → quiet 20 s),
 * derived from the newest trade's server timestamp and the clock in HELLO, so
 * it needs no clock sync with the browser.
 */
export function RegimeBadge() {
  const clock = useMarket((s) => s.clock);
  const ts = useMarket((s) => s.trades.list[0]?.ts);
  if (!clock || !ts) return null;
  const tick = Math.floor((ts - clock.epochMs) / clock.tickMs);
  const p = ((tick % clock.cycleTicks) + clock.cycleTicks) % clock.cycleTicks;
  const [regime, end, next] =
    p < clock.normalEnd
      ? (["NORMAL", clock.normalEnd, "burst"] as const)
      : p < clock.burstEnd
        ? (["BURST", clock.burstEnd, "quiet"] as const)
        : (["QUIET", clock.cycleTicks, "normal"] as const);
  const left = Math.ceil(((end - p) * clock.tickMs) / 1000);
  return (
    <div className="text-xs" title="Deterministic market cycle: every value is a function of the 50 ms market tick">
      <div className="text-muted">Market regime</div>
      <div className="num">
        <b className={LABEL[regime].cls}>{regime}</b> <span className="text-muted">{LABEL[regime].rate} · {next} in {left}s</span>
      </div>
    </div>
  );
}
