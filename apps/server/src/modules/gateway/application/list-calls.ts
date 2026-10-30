import { DomainError, err, ok, type Result } from '../../../shared-kernel/index.ts';
import type { CallOutcome } from '../domain/call-outcome.ts';
import type { CallLogEntry, GatewayDeps, Principal } from './ports.ts';

const DEFAULT_CALLS_LIMIT = 100;

/** The admin's view of the call log, newest first. Entries never carry arguments or results. */
export async function listCalls(
  deps: Pick<GatewayDeps, 'callLog'>,
  q: {
    principal: Principal;
    userId?: string;
    server?: string;
    outcome?: CallOutcome;
    before?: Date;
    limit?: number;
  },
): Promise<Result<(CallLogEntry & { id: string })[], DomainError>> {
  if (q.principal.role !== 'admin') return err(new DomainError('forbidden', 'administrators only'));
  const { principal: _principal, limit, ...filter } = q;
  const defined = Object.fromEntries(Object.entries(filter).filter(([, v]) => v !== undefined));
  return ok(await deps.callLog.list({ ...defined, limit: limit ?? DEFAULT_CALLS_LIMIT }));
}
