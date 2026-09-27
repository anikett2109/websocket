// Health report: round-trip measurement over the WebSocket.
//
// The app sends a binary PING carrying its own clock (performance.now() in
// microseconds); the server echoes it in a PONG, so RTT = now - echoedTimestamp
// uses only the browser's monotonic clock (no clock sync needed).
//
// Over the last W = 5 RTTs the app reports
//   latency = median(window)                    the typical round trip
//   jitter  = median(|RTT_i - latency|)  (MAD)  the typical deviation
// Both are robust: up to 2 of 5 samples can be outliers (a latency spike)
// without moving them. The backend owns the tier and scores
//   L = latency + 4 * jitter.

const WINDOW = 5;

const median = (xs: number[]) => {
  const s = [...xs].sort((a, b) => a - b);
  const m = s.length >> 1;
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
};

export class HealthMeter {
  private window: number[] = [];
  last = 0;
  samples = 0;

  add(rttMs: number) {
    if (!Number.isFinite(rttMs) || rttMs < 0) return;
    this.last = rttMs;
    this.samples++;
    this.window.push(rttMs);
    if (this.window.length > WINDOW) this.window.shift();
  }

  get latency() {
    return this.window.length ? median(this.window) : 0;
  }

  get jitter() {
    const l = this.latency;
    return this.window.length ? median(this.window.map((x) => Math.abs(x - l))) : 0;
  }

  reset() {
    this.window = [];
    this.last = this.samples = 0;
  }
}

/** Sliding-window event rate (events per second over the last `windowMs`). */
export class RateCounter {
  private times: number[] = [];
  constructor(private windowMs = 5000) {}
  hit(now: number) {
    this.times.push(now);
  }
  rate(now: number) {
    const cut = now - this.windowMs;
    while (this.times.length && this.times[0] < cut) this.times.shift();
    return (this.times.length * 1000) / this.windowMs;
  }
  reset() {
    this.times = [];
  }
}
