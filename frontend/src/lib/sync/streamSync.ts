// Generic "REST snapshot + ordered WebSocket deltas" synchroniser.
//
// Every delta packet carries baseSeq -> seq. The invariant is: a delta is only
// applied when baseSeq === local seq. Deltas are additive and may be coalesced
// (a slow tier sends 105->110 in one packet), so a packet can never be
// partially applied; synchronisation works like this:
//
//   start()        phase=snapshotting, request REST snapshot, buffer live deltas
//   onSnapshot(S)  drop buffered deltas with seq <= S; if the rest chain from S
//                  apply them and go live (fast path). Otherwise send SYNC(S):
//                  the server rebases this connection to S.
//   onSynced(S)    phase=live; every later delta on this ordered socket is based on S
//   live           base == seq -> apply; seq <= local -> duplicate, ignore;
//                  otherwise a gap -> start() again (re-snapshot)

export type Phase = "idle" | "snapshotting" | "syncing" | "live";

export class SyncError extends Error {}

export interface Seqd {
  seq: number;
}
export interface DeltaLike {
  seq: number;
  baseSeq: number;
}

export interface SyncIO<S> {
  requestSnapshot(token: number): void;
  sendSync(seq: number): void;
  onState(state: S): void;
  onPhase?(phase: Phase): void;
  onEvent?(msg: string, level: "info" | "warn"): void;
}

const MAX_BUFFER = 512;

export class StreamSync<S extends Seqd, D extends DeltaLike> {
  phase: Phase = "idle";
  state: S | null = null;
  private buffer: D[] = [];
  private token = 0;

  constructor(
    readonly name: string,
    private readonly apply: (s: S, d: D) => S,
    private readonly io: SyncIO<S>,
  ) {}

  get currentToken() {
    return this.token;
  }

  private setPhase(p: Phase) {
    if (this.phase !== p) {
      this.phase = p;
      this.io.onPhase?.(p);
    }
  }

  private event(msg: string, level: "info" | "warn" = "info") {
    this.io.onEvent?.(`${this.name}: ${msg}`, level);
  }

  /** Begin (or restart) synchronisation with a fresh REST snapshot. */
  start(reason: string) {
    this.token++;
    this.buffer = [];
    this.setPhase("snapshotting");
    this.event(`snapshot requested (${reason})`, reason === "initial" ? "info" : "warn");
    this.io.requestSnapshot(this.token);
  }

  /** Connection lost: stop applying, keep the last state (shown as stale). */
  stop() {
    this.token++;
    this.buffer = [];
    this.setPhase("idle");
  }

  onDelta(d: D) {
    switch (this.phase) {
      case "idle":
      case "syncing": // pre-rebase packets: the server will resend from our base after SYNCED
        return;
      case "snapshotting":
        this.buffer.push(d);
        if (this.buffer.length > MAX_BUFFER) this.buffer.shift();
        return;
      case "live": {
        const s = this.state!;
        if (d.baseSeq === s.seq) {
          this.applyOrResync(d);
        } else if (d.seq <= s.seq) {
          // duplicate / already covered: harmless
        } else {
          this.event(`gap detected: local seq ${s.seq}, packet ${d.baseSeq}->${d.seq}`, "warn");
          this.start("gap");
        }
      }
    }
  }

  private applyOrResync(d: D): boolean {
    try {
      this.state = this.apply(this.state!, d);
      this.io.onState(this.state);
      return true;
    } catch (e) {
      this.event(`delta rejected: ${(e as Error).message}`, "warn");
      this.start("invalid delta");
      return false;
    }
  }

  /** REST snapshot arrived. Late responses (older token) are ignored. */
  onSnapshot(snap: S, token: number) {
    if (token !== this.token || this.phase !== "snapshotting") return;
    this.state = snap;
    this.io.onState(snap);

    const pending = this.buffer.filter((d) => d.seq > snap.seq);
    const dropped = this.buffer.length - pending.length;
    this.buffer = [];
    let applied = 0;
    for (const d of pending) {
      if (d.baseSeq !== this.state.seq) break;
      if (!this.applyOrResync(d)) return;
      applied++;
    }
    if (applied > 0 && applied === pending.length) {
      this.event(`live from snapshot ${snap.seq} (+${applied} buffered, ${dropped} stale dropped)`, "info");
      this.setPhase("live");
      return;
    }
    // Buffer empty or it cannot chain from the snapshot: rebase the server.
    this.setPhase("syncing");
    this.io.sendSync(this.state.seq);
  }

  onSynced(seq: number) {
    if (this.phase !== "syncing") return;
    if (this.state && seq === this.state.seq) {
      this.event(`synced at seq ${seq}`, "info");
      this.setPhase("live");
    } else {
      this.start("sync ack mismatch");
    }
  }

  onSyncFailed(reason: string) {
    if (this.phase === "syncing") this.start(`sync failed: ${reason}`);
  }

  onResync() {
    if (this.phase !== "idle") this.start("server requested resync");
  }
}
