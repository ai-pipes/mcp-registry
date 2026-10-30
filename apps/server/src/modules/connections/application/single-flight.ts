import type { SingleFlight } from './ports.ts';

/** In-process single-flight: a second caller with the same key gets the first caller's promise. */
export function createSingleFlight<T>(): SingleFlight<T> {
  const inflight = new Map<string, Promise<T>>();
  return {
    run(key, fn) {
      const running = inflight.get(key);
      if (running) return running;
      const p = Promise.resolve()
        .then(fn)
        .finally(() => inflight.delete(key));
      inflight.set(key, p);
      return p;
    },
  };
}
