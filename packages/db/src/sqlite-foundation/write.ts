import { Database } from "bun:sqlite";
import { randomUUID } from "node:crypto";
import { encodeTimestamp, type Timestamp } from "./timestamp.js";

type Finite = Extract<Timestamp, { kind: "finite" }>;
export type WriteValue = Readonly<{ db: Database; timestamp: string; newUUID: () => string }>;

function currentTimestamp(): Finite {
  const millis = BigInt(Date.now());
  const remainder = ((millis % 1000n) + 1000n) % 1000n;
  return { kind: "finite", seconds: (millis - remainder) / 1000n, nanos: Number(remainder) * 1000000 };
}

// Synchronous callbacks only: do not retain the connection or manage boundaries.
// A returned thenable rolls back, but promises cannot be cancelled: delayed side
// effects can escape after rollback. Async/detached work is explicitly unsupported.
// Historical timestamps remain explicit values, never implicit SQL defaults.
export class SQLiteWriter {
  #active = false;
  readonly #db: Database;
  readonly #clock: () => Finite;

  constructor(db: Database, clock: () => Finite = currentTimestamp) {
    this.#db = db;
    this.#clock = clock;
  }

  write<T>(callback: (value: WriteValue) => T): T {
    if (this.#active || this.#db.inTransaction) throw new Error("Nested write transaction");
    if (Object.prototype.toString.call(callback) === "[object AsyncFunction]") {
      throw new Error("Async write callbacks are unsupported");
    }
    this.#active = true;
    try {
      return this.#db.transaction(() => {
        const clock = this.#clock();
        if (clock.kind !== "finite") throw new Error("Write clock must be finite");
        const value: WriteValue = Object.freeze({
          db: this.#db, timestamp: encodeTimestamp(clock)!, newUUID: randomUUID,
        });
        const result = callback(value);
        if (result !== null && (typeof result === "object" || typeof result === "function")
          && typeof (result as { then?: unknown }).then === "function") {
          throw new Error("Async write results are unsupported; delayed work cannot be cancelled");
        }
        return result;
      }).immediate();
    } finally {
      this.#active = false;
    }
  }
}
