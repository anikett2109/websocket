"use client";

import { useMarket, type Override, type TierName } from "@/store/market";
import { useFeed } from "./FeedProvider";

const TIER_STYLE: Record<TierName, string> = {
  FULL: "bg-up/15 text-up border-up/40",
  DEGRADED: "bg-warn/15 text-warn border-warn/40",
  MINIMAL: "bg-down/15 text-down border-down/40",
};

const perSec = (ms?: number) => (ms ? (1000 / ms).toFixed(ms >= 1000 ? 1 : 0) : "—");
const f1 = (n: number) => (Number.isFinite(n) ? n.toFixed(1) : "—");

export function TierPanel() {
  const tier = useMarket((s) => s.tier);
  const net = useMarket((s) => s.net);
  const measured = useMarket((s) => s.measured);
  const live = useMarket((s) => s.conn.status === "live");

  return (
    <section className="panel flex flex-col">
      <div className="flex items-center justify-between border-b border-line px-3 py-2">
        <h2 className="text-sm font-medium">Adaptive delivery</h2>
        <span className="text-xs text-muted">server-owned tier</span>
      </div>
      <div className="flex flex-col gap-3 p-3">
        <div className="flex items-center gap-3">
          <span className={`rounded-md border px-3 py-1 text-sm font-semibold ${tier.tier && live ? TIER_STYLE[tier.tier] : "border-line text-muted"}`}>
            {live && tier.tier ? tier.tier : "—"}
          </span>
          <span className="text-xs text-muted">
            {tier.override !== "AUTO" ? (
              <>forced by debug override (auto would be <b className="text-text">{tier.autoTier}</b>)</>
            ) : tier.warmedUp ? (
              "automatic"
            ) : (
              `warming up (${Math.min(net.samples, 3)}/3 probes)`
            )}
          </span>
        </div>
        {tier.reason && <div className="text-xs text-muted">Last change: {tier.reason}</div>}

        <table className="num w-full text-xs">
          <thead>
            <tr className="text-muted">
              <th className="text-left font-normal">Stream</th>
              <th className="text-right font-normal">Target</th>
              <th className="text-right font-normal">Target/s</th>
              <th className="text-right font-normal">Received/s</th>
            </tr>
          </thead>
          <tbody>
            {(
              [
                ["Depth", tier.rates?.depthMs, measured.depth],
                ["Chart", tier.rates?.chartMs, measured.chart],
                ["Trades", tier.rates?.tradeMs, measured.trades],
              ] as const
            ).map(([name, ms, got]) => (
              <tr key={name}>
                <td>{name}</td>
                <td className="text-right">{ms ? `${ms} ms` : "—"}</td>
                <td className="text-right">{perSec(ms)}</td>
                <td className="text-right">{got.toFixed(1)}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <p className="text-[11px] leading-snug text-muted">
          Received/s can be below target: the server never sends empty chart updates, and trades are only resent when a new trade exists.
        </p>

        <div className="grid grid-cols-2 gap-x-4 gap-y-1 border-t border-line pt-3 text-xs">
          <Metric label="Last RTT" v={`${f1(net.rttMs)} ms`} />
          <Metric label="Samples" v={`${net.samples}`} />
          <Metric label="Latency (median₅)" v={`${f1(net.latencyMs)} ms`} />
          <Metric label="Jitter (MAD₅)" v={`${f1(net.jitterMs)} ms`} />
        </div>
        <div className="flex justify-between rounded-md bg-panel-2 px-2 py-1.5 text-xs">
          <span className="text-muted">Server score L = latency + 4·jitter</span>
          <span className="num font-semibold">{f1(tier.serverEffectiveMs)} ms</span>
        </div>
        <Thresholds score={tier.serverEffectiveMs} />
      </div>
    </section>
  );
}

function Metric({ label, v, strong }: { label: string; v: string; strong?: boolean }) {
  return (
    <div className="flex justify-between gap-2">
      <span className="text-muted">{label}</span>
      <span className={`num ${strong ? "font-semibold" : ""}`}>{v}</span>
    </div>
  );
}

// Threshold = 2 × the tier's chart interval (at most one update in flight);
// promotion at 80 % of it (20 % dead band). Demote after 3 reports, promote after 5.
function Thresholds({ score }: { score: number }) {
  const rows: [TierName, string, string][] = [
    ["FULL", "L < 200 ms (2×100)", "back to FULL at L < 160 ×5"],
    ["DEGRADED", "200 ≤ L < 600 (2×300)", "demote at ≥ 200 ×3 / ≥ 600 ×3"],
    ["MINIMAL", "L ≥ 600 ms", "back to DEGRADED at L < 480 ×5"],
  ];
  const band: TierName = score < 200 ? "FULL" : score < 600 ? "DEGRADED" : "MINIMAL";
  return (
    <div className="rounded-md border border-line text-[11px]">
      {rows.map(([t, range, rule]) => (
        <div key={t} className={`flex justify-between gap-2 px-2 py-1 ${band === t ? "bg-panel-2" : ""}`}>
          <span className="w-20 font-medium">{t}</span>
          <span className="num text-muted">{range}</span>
          <span className="hidden text-muted sm:inline">{rule}</span>
        </div>
      ))}
    </div>
  );
}

export function DebugPanel() {
  const feed = useFeed();
  const override = useMarket((s) => s.tier.override);
  const sim = useMarket((s) => s.debug.simLatencyMs);
  const conn = useMarket((s) => s.conn);
  const live = conn.status === "live";

  const btn = "rounded-md border border-line px-2.5 py-1 text-xs hover:bg-panel-2 disabled:opacity-40";
  const seg = (on: boolean) => `rounded px-2.5 py-1 text-xs ${on ? "bg-line text-text" : "text-muted hover:text-text"}`;

  return (
    <section className="panel flex flex-col">
      <div className="flex items-center justify-between border-b border-line px-3 py-2">
        <h2 className="text-sm font-medium">Debug controls</h2>
        <span className="text-xs text-muted">demo only</span>
      </div>
      <div className="flex flex-col gap-3 p-3 text-xs">
        <div>
          <div className="mb-1 text-muted">Force tier</div>
          <div className="inline-flex flex-wrap rounded-md bg-panel-2 p-0.5">
            {(["AUTO", "FULL", "DEGRADED", "MINIMAL"] as Override[]).map((t) => (
              <button key={t} disabled={!live} className={seg(override === t)} onClick={() => feed?.setOverride(t)}>
                {t}
              </button>
            ))}
          </div>
        </div>
        <div>
          <div className="mb-1 text-muted">
            Simulated latency: added to every measured RTT before the health report, so the server&apos;s automatic tiering reacts. Presets aim L at the middle of each band.
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <div className="inline-flex flex-wrap rounded-md bg-panel-2 p-0.5">
              {(
                [
                  [0, "off"],
                  [300, "+300 ms → DEGRADED"],
                  [700, "+700 ms → MINIMAL"],
                ] as const
              ).map(([ms, label]) => (
                <button key={ms} disabled={!live} className={seg(sim === ms)} onClick={() => feed?.setSimLatency(ms)}>
                  {label}
                </button>
              ))}
            </div>
            <button className={btn} disabled={!live} onClick={() => feed?.spike()} title="Adds 900 ms to one RTT sample; the median ignores it">
              Spike (tier should hold)
            </button>
          </div>
        </div>
        <div>
          <div className="mb-1 text-muted">Fault injection (server skips one packet → client detects gap and re-snapshots)</div>
          <div className="flex flex-wrap gap-2">
            <button className={btn} disabled={!live} onClick={() => feed?.dropPacket("depth")}>Drop depth packet</button>
            <button className={btn} disabled={!live} onClick={() => feed?.dropPacket("chart")}>Drop chart packet</button>
          </div>
        </div>
        <div>
          <div className="mb-1 text-muted">Connection</div>
          <div className="flex flex-wrap gap-2">
            <button className={btn} disabled={!live} onClick={() => feed?.kill()}>Kill socket (auto-reconnect)</button>
            <button className={btn} onClick={() => feed?.setPaused(!conn.paused)}>
              {conn.paused ? "Resume connection" : "Go offline"}
            </button>
          </div>
        </div>
      </div>
    </section>
  );
}

export function EventLog() {
  const events = useMarket((s) => s.events);
  return (
    <section className="panel flex min-h-0 flex-col">
      <div className="border-b border-line px-3 py-2">
        <h2 className="text-sm font-medium">Sync &amp; connection log</h2>
      </div>
      <ol className="num max-h-64 overflow-y-auto p-2 text-[11px]">
        {events.length === 0 && <li className="p-2 text-muted">No events yet.</li>}
        {events.map((e) => (
          <li key={e.id} className="flex gap-2 px-1 py-0.5">
            <span className="shrink-0 text-muted">{new Date(e.at).toLocaleTimeString()}</span>
            <span className={e.level === "warn" ? "text-warn" : "text-text"}>{e.msg}</span>
          </li>
        ))}
      </ol>
    </section>
  );
}
