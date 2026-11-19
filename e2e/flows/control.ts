// Client of the control API of the Go orchestrator (compat/e2e/control.go). Every call carries
// the run's bearer token. The API knows fixed operations only: there is no SQL or free-form CLI
// in it, so there is none here. Answers are checked, not assumed: a changed shape fails the flow
// at the call instead of somewhere later.

export interface StackInfo {
  baseURL: string;
  oidc: boolean;
  providerURL: string;
}

interface RegisteredServer {
  slug: string;
  name: string;
  url: string;
  tools: string[];
}

interface UpstreamCall {
  tool: string;
  arguments: unknown;
  headerNames: string[];
}

export type StackName = 'plain' | 'oidc' | 'samesite';

const STACK_NAMES: readonly StackName[] = ['plain', 'oidc', 'samesite'];

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function isStringArray(v: unknown): v is string[] {
  return Array.isArray(v) && v.every((x) => typeof x === 'string');
}

function field(v: unknown, name: string): unknown {
  if (!isRecord(v)) throw new Error(`control answer is not an object: ${JSON.stringify(v)}`);
  return v[name];
}

function str(v: unknown, name: string): string {
  const x = field(v, name);
  if (typeof x !== 'string') throw new Error(`control answer has no string "${name}"`);
  return x;
}

class Control {
  private async call(method: 'GET' | 'POST', path: string, body?: unknown): Promise<unknown> {
    const url = process.env['E2E_CONTROL_URL'];
    const token = process.env['E2E_CONTROL_TOKEN'];
    if (url === undefined || token === undefined) {
      throw new Error(
        'E2E_CONTROL_URL and E2E_CONTROL_TOKEN are not set: run the flows through `make e2e-flows`',
      );
    }
    const res = await fetch(url + path, {
      method,
      headers: {
        Authorization: `Bearer ${token}`,
        ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
      },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    const text = await res.text();
    if (!res.ok) throw new Error(`control ${method} ${path}: ${res.status} ${text}`);
    const parsed: unknown = JSON.parse(text);
    return parsed;
  }

  async stacks(): Promise<Record<StackName, StackInfo>> {
    const all = field(await this.call('GET', '/stacks'), 'stacks');
    const read = (name: StackName): StackInfo => {
      const s = field(all, name);
      const oidc = field(s, 'oidc');
      if (typeof oidc !== 'boolean') throw new Error(`stack ${name}: no boolean "oidc"`);
      return { baseURL: str(s, 'baseURL'), oidc, providerURL: str(s, 'providerURL') };
    };
    const [plain, oidc, samesite] = STACK_NAMES.map(read);
    if (plain === undefined || oidc === undefined || samesite === undefined) {
      throw new Error('control answer lacks a stack');
    }
    return { plain, oidc, samesite };
  }

  async createUser(stack: StackName, sub: string, name: string, admin: boolean): Promise<void> {
    await this.call('POST', '/cli/create-user', { stack, sub, name, admin });
  }

  async loginLink(stack: StackName, sub: string): Promise<string> {
    return str(await this.call('POST', '/cli/login-link', { stack, sub }), 'link');
  }

  async firstStartLink(stack: StackName): Promise<string> {
    return str(await this.call('POST', '/stack/first-start-link', { stack }), 'link');
  }

  async nextLogin(claims: {
    sub: string;
    name: string;
    email: string;
    groups: string[];
  }): Promise<void> {
    await this.call('POST', '/idp/next-login', claims);
  }

  async registerServer(
    stack: StackName,
    kind: 'none' | 'shared_secret' | 'pat_user' | 'oauth_user',
    options: { slug?: string; tools?: string[] } = {},
  ): Promise<RegisteredServer> {
    const r = await this.call('POST', '/servers', {
      stack,
      kind,
      slug: options.slug ?? '',
      tools: options.tools ?? [],
    });
    const tools = field(r, 'tools');
    if (!isStringArray(tools)) throw new Error('registered server has no tools list');
    return { slug: str(r, 'slug'), name: str(r, 'name'), url: str(r, 'url'), tools };
  }

  async upstreamCalls(slug: string): Promise<UpstreamCall[]> {
    const r = await this.call('GET', `/upstream/calls?slug=${encodeURIComponent(slug)}`);
    const calls = field(r, 'calls');
    if (!Array.isArray(calls)) throw new Error('upstream calls answer has no list');
    return calls.map((c: unknown): UpstreamCall => {
      const headerNames = field(c, 'headerNames');
      if (!isStringArray(headerNames)) throw new Error('upstream call has no header names');
      return { tool: str(c, 'tool'), arguments: field(c, 'arguments'), headerNames };
    });
  }

  /**
   * Rows of the audit table (read-only), newest first, filtered by the sub of the actor and/or the
   * action. For a flow that needs a specific record: the image's audit list is ordered by the text
   * form of the id, so the first page of the UI does not reliably hold the newest rows.
   */
  async audit(
    stack: StackName,
    filter: { actor?: string; action?: string } = {},
  ): Promise<{ action: string; target: string; actorSub: string }[]> {
    const q = new URLSearchParams({
      stack,
      actor: filter.actor ?? '',
      action: filter.action ?? '',
    });
    const rows = field(await this.call('GET', `/audit?${q.toString()}`), 'rows');
    if (!Array.isArray(rows)) throw new Error('audit answer has no rows');
    return rows.map((r: unknown) => ({
      action: str(r, 'action'),
      target: str(r, 'target'),
      actorSub: str(r, 'actorSub'),
    }));
  }

  async expireSession(stack: StackName, sub: string): Promise<void> {
    await this.call('POST', '/sessions/expire', { stack, sub });
  }
}

/** The one client of the run. */
export const control = new Control();

let stacksCache: Promise<Record<StackName, StackInfo>> | undefined;

/** Base URLs of the stacks (asked once per worker). */
export function getStacks(): Promise<Record<StackName, StackInfo>> {
  stacksCache ??= control.stacks();
  return stacksCache;
}

export interface Person {
  sub: string;
  name: string;
  link: string;
}

/** Creates a person on a stack and a sign-in link for them. */
export async function newPerson(stack: StackName, name: string, admin: boolean): Promise<Person> {
  const sub = `e2e-${Math.random().toString(36).slice(2, 10)}`;
  await control.createUser(stack, sub, name, admin);
  return { sub, name, link: await control.loginLink(stack, sub) };
}
