// Round-trip measurement over the WebSocket, RFC 6298 style.
//
// Every second the app sends a binary PING carrying its own clock
// (performance.now() in microseconds). The server echoes it unchanged in a
// PONG, so RTT = now - echoedTimestamp uses only the browser's monotonic clock
// (no clock-sync needed between browser and server).
//
//   first sample:  SRTT = RTT, RTTVAR = RTT / 2
//   afterwards:    RTTVAR = (1 - 1/4) * RTTVAR + 1/4 * |SRTT - RTT|
//                  SRTT   = (1 - 1/8) * SRTT   + 1/8 * RTT
//
// "Jitter" is reported as RTTVAR (the smoothed mean deviation of RTT).
// The backend combines them as EffectiveLatency = SRTT + 4 * RTTVAR.

export const ALPHA = 1 / 8;
export const BETA = 1 / 4;

export class LatencyMeter {
  srtt = 0;
  rttvar = 0;
  last = 0;
  samples = 0;

  add(rttMs: number) {
    if (!Number.isFinite(rttMs) || rttMs < 0) return;
    this.last = rttMs;
    if (this.samples === 0) {
      this.srtt = rttMs;
      this.rttvar = rttMs / 2;
    } else {
      this.rttvar = (1 - BETA) * this.rttvar + BETA * Math.abs(this.srtt - rttMs);
      this.srtt = (1 - ALPHA) * this.srtt + ALPHA * rttMs;
    }
    this.samples++;
  }

  get effective() {
    return this.srtt + 4 * this.rttvar;
  }

  reset() {
    this.srtt = this.rttvar = this.last = this.samples = 0;
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
