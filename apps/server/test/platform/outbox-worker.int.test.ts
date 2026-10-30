import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vite-plus/test';
import { createLogger } from '../../src/platform/logger.ts';
import { appendOutbox, MAX_ATTEMPTS } from '../../src/platform/outbox.ts';
import { startOutboxWorker } from '../../src/platform/outbox-worker.ts';
import { startPg, truncateAll } from '../pg.ts';

let pg: Awaited<ReturnType<typeof startPg>>;
let worker: { stop(): Promise<void> } | null = null;

beforeAll(async () => {
  pg = await startPg();
});
afterAll(async () => {
  await pg.stop();
});
beforeEach(async () => {
  await truncateAll(pg.sql);
});
afterEach(async () => {
  await worker?.stop();
  worker = null;
});

type Handlers = Parameters<typeof startOutboxWorker>[0]['handlers'];

function start(handlers: Handlers, logger = createLogger('silent')) {
  worker = startOutboxWorker({ sql: pg.sql, handlers, intervalMs: 20, logger });
  return worker;
}

const event = (type: string) => ({ type, occurredAt: new Date(), payload: { n: 1 } });
const publish = (type: string) => pg.sql.begin((tx) => appendOutbox(tx, [event(type)]));

async function until(check: () => Promise<boolean>, timeoutMs = 5000): Promise<void> {
  if (await check()) return;
  if (timeoutMs <= 0) throw new Error('condition not met in time');
  await new Promise((r) => setTimeout(r, 10));
  await until(check, timeoutMs - 10);
}

type OutboxState = { attempts: number; processed: boolean; last_error: string | null };
const outbox = () =>
  pg.sql<OutboxState[]>`
    select attempts, processed_at is not null as processed, last_error
    from platform.outbox order by id`;

describe('outbox worker', () => {
  it('delivers an event to its handler and marks it processed', async () => {
    const seen: Record<string, unknown>[] = [];
    await publish('test.happened');
    start({
      'test.happened': async (payload) => {
        seen.push(payload);
      },
    });
    await until(async () => (await outbox())[0]?.processed === true);
    expect(seen).toEqual([{ n: 1 }]);
    expect(await outbox()).toEqual([{ attempts: 1, processed: true, last_error: null }]);
  });

  it('marks events without a handler as processed', async () => {
    await publish('unknown.event');
    start({});
    await until(async () => (await outbox())[0]?.processed === true);
    expect(await outbox()).toEqual([{ attempts: 1, processed: true, last_error: null }]);
  });

  it('keeps a failing event for retry with a content-free error label', async () => {
    let fail = true;
    await publish('test.flaky');
    const w = start({
      'test.flaky': async () => {
        if (fail) throw new Error('secret-value-xyz');
      },
    });
    await until(async () => (await outbox())[0]?.last_error != null);
    await w.stop();
    expect(await outbox()).toEqual([{ attempts: 1, processed: false, last_error: 'Error' }]);

    fail = false;
    await pg.sql`update platform.outbox set next_attempt_at = now() - interval '1 second'`;
    start({
      'test.flaky': async () => {
        if (fail) throw new Error('unreachable');
      },
    });
    await until(async () => (await outbox())[0]?.processed === true);
    expect(await outbox()).toEqual([{ attempts: 2, processed: true, last_error: null }]);
  });

  it('logs an error on the last attempt, without the error message', async () => {
    await publish('test.broken');
    await pg.sql`update platform.outbox set attempts = ${MAX_ATTEMPTS - 1}`;
    const lines: Record<string, unknown>[] = [];
    const log = createLogger('info', {
      write: (line: string) => {
        const parsed: unknown = JSON.parse(line);
        if (typeof parsed === 'object' && parsed !== null) lines.push({ ...parsed });
      },
    });
    start(
      {
        'test.broken': async () => {
          throw new Error('secret-value-xyz');
        },
      },
      log,
    );
    await until(async () => lines.some((l) => l['msg'] === 'outbox: handler failed, giving up'));
    expect(JSON.stringify(lines)).not.toContain('secret-value-xyz');
    expect(lines.find((l) => l['msg'] === 'outbox: handler failed, giving up')).toMatchObject({
      level: 50,
      type: 'test.broken',
      attempts: MAX_ATTEMPTS,
    });
  });

  it('runs the sweep at most once per period', async () => {
    let runs = 0;
    worker = startOutboxWorker({
      sql: pg.sql,
      handlers: {},
      intervalMs: 10,
      logger: createLogger('silent'),
      sweep: {
        everyMs: 60_000,
        run: async () => {
          runs += 1;
        },
      },
    });
    await until(async () => runs === 1);
    await new Promise((r) => setTimeout(r, 100));
    expect(runs).toBe(1);
  });

  it('stop() waits for the running tick and ends the loop', async () => {
    let calls = 0;
    const w = start({
      'test.slow': async () => {
        calls += 1;
        await new Promise((r) => setTimeout(r, 100));
      },
    });
    await publish('test.slow');
    await until(async () => calls === 1);
    await w.stop();
    expect(await outbox()).toEqual([{ attempts: 1, processed: true, last_error: null }]);
    await publish('test.slow');
    await new Promise((r) => setTimeout(r, 100));
    expect(calls).toBe(1);
  });
});
