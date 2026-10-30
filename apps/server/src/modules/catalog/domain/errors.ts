import { DomainError } from '../../../shared-kernel/index.ts';

export type CatalogErrorCode =
  | 'invalid_slug'
  | 'invalid_tool_ref'
  | 'invalid_auth'
  | 'invalid_access'
  | 'invalid_server'
  | 'server_exists'
  /** The server was changed by someone else between reading and writing it. */
  | 'server_conflict'
  | 'forbidden'
  | 'not_found'
  | 'needs_connection'
  | 'sync_failed';

export class CatalogError extends DomainError {
  declare readonly code: CatalogErrorCode;

  constructor(code: CatalogErrorCode, message: string) {
    super(code, message);
    this.name = 'CatalogError';
  }
}
