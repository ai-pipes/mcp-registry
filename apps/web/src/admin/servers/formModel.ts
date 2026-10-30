import {
  CreateServerRequest,
  UpdateServerRequest,
  type AuthInput,
  type ServerView,
} from '@mcp/contracts';
import type { ZodIssue } from 'zod';
import { strings } from '@/strings.ts';

export type AuthKind = AuthInput['kind'];

export interface FormState {
  slug: string;
  name: string;
  description: string;
  url: string;
  kind: AuthKind;
  header: string;
  template: string;
  authorizeUrl: string;
  tokenUrl: string;
  revokeUrl: string;
  clientId: string;
  scopes: string;
  params: { key: string; value: string }[];
  secret: string;
  accessKind: 'all' | 'groups';
  groups: string;
  timeoutMs: string;
  dailyLimit: string;
}

export type FieldName =
  | 'slug'
  | 'name'
  | 'description'
  | 'url'
  | 'kind'
  | 'header'
  | 'template'
  | 'authorizeUrl'
  | 'tokenUrl'
  | 'revokeUrl'
  | 'clientId'
  | 'scopes'
  | 'params'
  | 'secret'
  | 'groups'
  | 'timeoutMs'
  | 'dailyLimit';

export type FieldErrors = Partial<Record<FieldName, string>>;

const FIELD_NAMES: readonly FieldName[] = [
  'slug',
  'name',
  'description',
  'url',
  'header',
  'template',
  'authorizeUrl',
  'tokenUrl',
  'revokeUrl',
  'clientId',
  'scopes',
  'secret',
  'groups',
  'timeoutMs',
  'dailyLimit',
];
const isFieldName = (p: PropertyKey): p is FieldName => FIELD_NAMES.some((n) => n === p);

export const AUTH_KINDS: AuthKind[] = ['none', 'shared_secret', 'oauth_user', 'pat_user'];
export const isAuthKind = (v: unknown): v is AuthKind => AUTH_KINDS.some((k) => k === v);
const DEFAULT_HEADER = { header: 'Authorization', template: 'Bearer {secret}' };

export function initialState(server?: ServerView): FormState {
  const a = server?.auth;
  const oauth = a?.kind === 'oauth_user' ? a.oauth : undefined;
  return {
    slug: server?.slug ?? '',
    name: server?.name ?? '',
    description: server?.description ?? '',
    url: server?.url ?? '',
    kind: a?.kind ?? 'none',
    header: a && a.kind !== 'none' ? a.header : DEFAULT_HEADER.header,
    template: a && a.kind !== 'none' ? a.template : DEFAULT_HEADER.template,
    authorizeUrl: oauth?.authorizeUrl ?? '',
    tokenUrl: oauth?.tokenUrl ?? '',
    revokeUrl: oauth?.revokeUrl ?? '',
    clientId: oauth?.clientId ?? '',
    scopes: oauth?.scopes.join(' ') ?? '',
    params: Object.entries(oauth?.extraAuthorizeParams ?? {}).map(([key, value]) => ({
      key,
      value,
    })),
    secret: '',
    accessKind: server?.access.kind ?? 'all',
    groups: server?.access.kind === 'groups' ? server.access.groups.join(', ') : '',
    timeoutMs: String(server?.timeoutMs ?? 120_000),
    dailyLimit: server?.dailyLimit === null || !server ? '' : String(server.dailyLimit),
  };
}

const words = (s: string) => s.split(/[\s,]+/).filter((w) => w !== '');

function authOf(f: FormState): unknown {
  if (f.kind === 'none') return { kind: 'none' };
  const header = { header: f.header, template: f.template };
  if (f.kind !== 'oauth_user') return { kind: f.kind, ...header };
  const extra = Object.fromEntries(
    f.params.filter((p) => p.key.trim() !== '').map((p) => [p.key.trim(), p.value]),
  );
  return {
    kind: 'oauth_user',
    ...header,
    oauth: {
      authorizeUrl: f.authorizeUrl,
      tokenUrl: f.tokenUrl,
      ...(f.revokeUrl.trim() === '' ? {} : { revokeUrl: f.revokeUrl }),
      clientId: f.clientId,
      scopes: words(f.scopes),
      ...(Object.keys(extra).length === 0 ? {} : { extraAuthorizeParams: extra }),
    },
  };
}

function describeIssue(issue: ZodIssue): string {
  if (issue.code === 'too_small' && issue.minimum === 1) return strings.form.required;
  if (issue.code === 'too_small') return strings.form.tooSmall;
  if (issue.code === 'too_big') return strings.form.tooBig;
  if (issue.code === 'invalid_type') {
    const field = issue.path.at(-1);
    return field === 'timeoutMs' || field === 'dailyLimit'
      ? strings.form.invalidNumber
      : strings.form.required;
  }
  if (issue.code === 'invalid_format' || /http/.test(issue.message)) return strings.form.invalidUrl;
  return strings.form.invalid;
}

function fieldOf(path: readonly PropertyKey[]): FieldName | undefined {
  // `access.groups.0` and `auth.oauth.scopes.2` end in an index; the field is the last name.
  return path.toReversed().find(isFieldName);
}

export type Built =
  | { ok: true; body: Record<string, unknown>; secret: string | undefined }
  | { ok: false; errors: FieldErrors };

/** The request body for the form, or the schema errors keyed by field. */
export function build(f: FormState, mode: 'create' | 'edit'): Built {
  const body: Record<string, unknown> = {
    ...(mode === 'create' ? { slug: f.slug.trim() } : {}),
    name: f.name.trim(),
    description: f.description.trim(),
    url: f.url.trim(),
    auth: authOf(f),
    access: f.accessKind === 'all' ? { kind: 'all' } : { kind: 'groups', groups: words(f.groups) },
    timeoutMs: f.timeoutMs.trim() === '' ? undefined : Number(f.timeoutMs),
    dailyLimit: f.dailyLimit.trim() === '' ? null : Number(f.dailyLimit),
  };
  const secret = f.secret === '' ? undefined : f.secret;
  const schema = mode === 'create' ? CreateServerRequest : UpdateServerRequest;
  const parsed = schema.safeParse({ ...body, ...(secret === undefined ? {} : { secret }) });
  if (parsed.success) return { ok: true, body: parsed.data, secret };
  const errors: FieldErrors = {};
  for (const issue of parsed.error.issues) {
    const field = fieldOf(issue.path);
    if (field && errors[field] === undefined) errors[field] = describeIssue(issue);
  }
  if (Object.keys(errors).length === 0) errors.name = strings.form.invalid;
  return { ok: false, errors };
}

/** Maps a server rejection (`ErrorBody`, English message) to the field it is about. */
export function serverErrorField(
  code: string,
  message: string,
): { field: FieldName; text: string } | undefined {
  if (code === 'server_exists') return { field: 'slug', text: strings.form.slugTaken };
  const prefix = /^([\w.]+): /.exec(message);
  const fromPath = prefix ? fieldOf(prefix[1]!.split('.')) : undefined;
  if (fromPath) return { field: fromPath, text: message.slice(prefix![0].length) };
  if (code === 'invalid_auth') {
    if (/scope/i.test(message)) return { field: 'scopes', text: message };
    if (/template|secret/i.test(message)) return { field: 'template', text: message };
    if (/header/i.test(message)) return { field: 'header', text: message };
    return { field: 'kind', text: message };
  }
  if (/url/i.test(message)) return { field: 'url', text: message };
  if (/slug/i.test(message)) return { field: 'slug', text: message };
  return undefined;
}
