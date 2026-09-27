// Fixed-point helpers. Market values stay integers (price x100, qty x1e6) end
// to end; they are only turned into strings for display, never into floats for math.

const PRICE_SCALE = 100;
const QTY_SCALE = 1_000_000;

const group = (s: string) => s.replace(/\B(?=(\d{3})+(?!\d))/g, ",");

/** Format a fixed-point integer with `decimals` fraction digits (truncating extra precision). */
export function formatFixed(value: number, scale: number, decimals: number, grouping = true): string {
  const neg = value < 0;
  const abs = Math.abs(value);
  const intPart = Math.floor(abs / scale);
  const scaleDigits = Math.round(Math.log10(scale));
  let frac = String(abs % scale).padStart(scaleDigits, "0").slice(0, decimals);
  frac = frac.padEnd(decimals, "0");
  const i = grouping ? group(String(intPart)) : String(intPart);
  return (neg ? "-" : "") + i + (decimals > 0 ? "." + frac : "");
}

export const fmtPrice = (v: number) => formatFixed(v, PRICE_SCALE, 2);
export const fmtQty = (v: number, decimals = 4) => formatFixed(v, QTY_SCALE, decimals);

/** Percentage change a->b with 2 decimals, computed on integers (basis points). */
export function fmtChangePct(from: number, to: number): string {
  if (!from) return "0.00%";
  const bp = Math.round(((to - from) * 10_000) / from); // basis points
  const sign = bp > 0 ? "+" : bp < 0 ? "-" : "";
  return sign + formatFixed(Math.abs(bp), 100, 2, false) + "%";
}

/** Chart rendering is the one place values become floats (the library needs numbers). */
export const toChartPrice = (v: number) => v / PRICE_SCALE;
export const toChartQty = (v: number) => v / QTY_SCALE;

export function fmtTime(ms: number, withMs = false): string {
  const d = new Date(ms);
  const p = (n: number, w = 2) => String(n).padStart(w, "0");
  const base = `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  return withMs ? `${base}.${p(d.getMilliseconds(), 3)}` : base;
}

export function fmtDateTime(ms: number): string {
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${fmtTime(ms)}`;
}
