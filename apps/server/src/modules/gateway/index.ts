// Public API of the gateway module: the only file other modules and the platform may import.
import type { Sql } from '../../platform/db.ts';
import type { GatewayDeps } from './application/ports.ts';
import { PgCallLog } from './infrastructure/pg-call-log.ts';
import { PgQuota } from './infrastructure/pg-quota.ts';

export { CALL_TOOL_TEXT, callTool } from './application/call-tool.ts';
export { listCalls } from './application/list-calls.ts';
export { pruneGatewayHistory } from './application/retention.ts';
export type {
  ArgsValidator,
  CallLog,
  CallLogEntry,
  CallLogFilter,
  GatewayDeps,
  GatewayLogger,
  PersonalCredentials,
  Principal,
  Quota,
  ToolCatalog,
} from './application/ports.ts';
export { AgentConnection } from './domain/agent-connection.ts';
export { CallOutcome } from './domain/call-outcome.ts';
export { QuotaDay } from './domain/quota-day.ts';
export { AjvArgsValidator } from './infrastructure/ajv-args-validator.ts';
export { createCatalogToolCatalog } from './infrastructure/catalog-tools.ts';
export { createConnectionCredentials } from './infrastructure/connection-credentials.ts';

export function createPgGateway(sql: Sql): Pick<GatewayDeps, 'callLog' | 'quota'> {
  return { callLog: new PgCallLog(sql), quota: new PgQuota(sql) };
}
export { callRoutes } from './interfaces/http/calls.ts';
