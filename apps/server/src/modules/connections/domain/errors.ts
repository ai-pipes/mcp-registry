import { DomainError } from '../../../shared-kernel/index.ts';

export type ConnectionErrorCode =
  /** No such server, not active, or the person may not use it. */
  | 'not_found'
  /** The server does not take this kind of personal credential (OAuth vs PAT). */
  | 'invalid_auth_kind'
  /** The connect attempt is unknown, used, expired or someone else's. */
  | 'invalid_state'
  /** The OAuth provider refused to exchange the code. */
  | 'exchange_failed'
  | 'invalid_pat';

export class ConnectionError extends DomainError {
  constructor(
    override readonly code: ConnectionErrorCode,
    message: string,
  ) {
    super(code, message);
    this.name = 'ConnectionError';
  }
}
