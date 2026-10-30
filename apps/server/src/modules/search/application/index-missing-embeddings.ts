import { indexTools } from './index-tools.ts';
import type { SearchDeps } from './ports.ts';

/** Consecutive failures that make the sweep treat the embedder as down and stop. */
const OUTAGE_STREAK = 3;
/** Known-good passage: tells a poison streak from an embedder outage. */
const PROBE_TEXT = 'health probe';
/** Wall-clock budget of one sweep, well under the 60 s outbox lease. */
const SWEEP_BUDGET_MS = 30_000;

export type SweepResult = {
  indexed: string[];
  /** Refs whose embedding failed (marked or not). Refs only, never content. */
  failed: string[];
  stoppedEarly: 'outage' | 'budget' | null;
};

/**
 * Recovery sweep: re-indexes up to `limit` tool documents whose embedding is null, e.g. after an
 * embedder outage outlasted the outbox retries. Does nothing until the embedder is ready.
 * Sequential. A failure is an isolated "poison document" only if a later document succeeds;
 * then it is marked (`embed_failed_at`) and skipped for an hour. After a streak of
 * OUTAGE_STREAK failures a known-good probe passage is embedded: if it succeeds the streak is
 * poison (all marked, sweep continues); if it fails the embedder is down (sweep stops, nothing
 * marked). The sweep also stops when the wall-clock budget is spent. Documents not attempted
 * are never marked.
 */
export async function indexMissingEmbeddings(
  deps: SearchDeps,
  opts: { limit: number; budgetMs?: number; now?: () => number },
): Promise<SweepResult> {
  const result: SweepResult = { indexed: [], failed: [], stoppedEarly: null };
  if (!(await deps.embedder.ready())) return result;
  const now = opts.now ?? Date.now;
  const deadline = now() + (opts.budgetMs ?? SWEEP_BUDGET_MS);
  const pending = await deps.index.withoutEmbedding(opts.limit);
  // Failures since the last success; marked only once a later document succeeds.
  let streak: string[] = [];
  await pending.reduce(async (prev, { ref, server }) => {
    await prev;
    if (result.stoppedEarly !== null) return;
    if (now() >= deadline) {
      result.stoppedEarly = 'budget';
      return;
    }
    try {
      await indexTools(deps, { server, refs: [ref] });
      result.indexed.push(ref);
      const isolated = streak;
      streak = [];
      await Promise.all(isolated.map((r) => deps.index.markEmbedFailed(r)));
    } catch {
      result.failed.push(ref);
      streak.push(ref);
      if (streak.length >= OUTAGE_STREAK) {
        const healthy = await deps.embedder.embedPassages([PROBE_TEXT]).then(
          () => true,
          () => false,
        );
        if (healthy) {
          const poison = streak;
          streak = [];
          await Promise.all(poison.map((r) => deps.index.markEmbedFailed(r)));
        } else {
          result.stoppedEarly = 'outage';
        }
      }
    }
  }, Promise.resolve());
  return result;
}
