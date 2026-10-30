import { describe, expect, it } from 'vite-plus/test';
import { errorFields, errorLabel } from './error-fields.ts';
import { UpstreamError } from './upstream.ts';

describe('UpstreamError.rejected', () => {
  it('keeps the JSON-RPC code and message apart from the error message', () => {
    const e = UpstreamError.rejected(-32602, 'owner "secret-xyz" not found');
    expect(e.kind).toBe('rejected');
    expect(e.rejection).toEqual({ code: -32602, message: 'owner "secret-xyz" not found' });
    expect(e.message).toBe('upstream rejected the request (JSON-RPC -32602)');
    expect(JSON.stringify([errorFields(e), errorLabel(e), String(e)])).not.toContain('secret-xyz');
  });

  it('other kinds carry no rejection', () => {
    expect(new UpstreamError('timeout').rejection).toBeNull();
  });
});
