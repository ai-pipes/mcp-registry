import { syncAllServers } from '../modules/catalog/index.ts';
import { pruneGatewayHistory } from '../modules/gateway/index.ts';
import { type IdentityDeps, pruneExpiredSessions } from '../modules/identity/index.ts';
import { errorFields } from '../shared-kernel/index.ts';
import type { Container } from './container.ts';

const HOUR_MS = 3_600_000;
const DAY_MS = 24 * HOUR_MS;
/** setTimeout fires at once for delays above 2^31-1 ms (~24.8 days); longer waits are chained. */
const MAX_TIMER_MS = 2 ** 31 - 1;

type SchedulerDeps = Pick<Container, 'catalogSync' | 'logger' | 'clock'> & {
  gateway: Pick<Container['gateway'], 'callLog' | 'quota'>;
  identity: { sessions: Pick<IdentityDeps['sessions'], 'deleteDead'> };
  config: Pick<Container['config'], 'syncIntervalHours' | 'callLogRetentionDays'>;
};

type Job = { stop(): Promise<void> };

/** Runs `job` now and then `intervalMs` after each run ends. A job must not throw. */
function every(intervalMs: number, job: () => Promise<void>): Job {
  let stopped = false;
  let timer: NodeJS.Timeout | undefined;
  let running: Promise<void> = Promise.resolve();

  const wait = (remainingMs: number): void => {
    if (stopped) return;
    const step = Math.min(remainingMs, MAX_TIMER_MS);
    timer = setTimeout(() => {
      if (remainingMs > step) wait(remainingMs - step);
      else run();
    }, step);
  };
  const run = (): void => {
    running = job().then(() => wait(intervalMs));
  };
  run();

  return {
    async stop() {
      stopped = true;
      clearTimeout(timer);
      await running;
    },
  };
}

/**
 * Periodic jobs, each at start and then on its own period; a failing run is logged and the
 * schedule goes on. Catalog synchronisation of every active server every `syncIntervalHours`;
 * gateway retention (call log older than `callLogRetentionDays`, old quota days) daily.
 * `stop()` cancels the next runs and waits for the running ones.
 */
export function startScheduler(c: SchedulerDeps): { stop(): Promise<void> } {
  const syncAll = async (): Promise<void> => {
    try {
      const outcomes = await syncAllServers(c.catalogSync);
      const failed = outcomes.flatMap((o) => (o.ok ? [] : [{ server: o.slug, code: o.code }]));
      c.logger.info({ servers: outcomes.length, failed }, 'catalog sync: done');
    } catch (error) {
      c.logger.error({ err: errorFields(error) }, 'catalog sync: run failed');
    }
  };

  const pruneGateway = async (): Promise<void> => {
    try {
      const deleted = await pruneGatewayHistory(
        { ...c.gateway, clock: c.clock },
        { callLogRetentionDays: c.config.callLogRetentionDays },
      );
      c.logger.info(deleted, 'gateway retention: done');
    } catch (error) {
      c.logger.error({ err: errorFields(error) }, 'gateway retention: run failed');
    }
  };

  const pruneSessions = async (): Promise<void> => {
    try {
      const sessions = await pruneExpiredSessions({
        sessions: c.identity.sessions,
        clock: c.clock,
      });
      c.logger.info({ sessions }, 'session retention: done');
    } catch (error) {
      c.logger.error({ err: errorFields(error) }, 'session retention: run failed');
    }
  };

  const jobs = [
    every(c.config.syncIntervalHours * HOUR_MS, syncAll),
    every(DAY_MS, pruneGateway),
    every(DAY_MS, pruneSessions),
  ];
  return {
    async stop() {
      await Promise.all(jobs.map((j) => j.stop()));
    },
  };
}
