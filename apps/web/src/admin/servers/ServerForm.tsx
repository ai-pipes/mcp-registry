import { ServerView } from '@mcp/contracts';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { PlusIcon, TriangleAlertIcon, XIcon } from 'lucide-react';
import { useId, useState, type FormEvent, type ReactNode } from 'react';
import { api } from '@/api/client.ts';
import { isApiError } from '@/api/errors.ts';
import { queryKeys } from '@/api/queries.ts';
import { ErrorAlert } from '@/components/ErrorAlert.tsx';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group';
import { Textarea } from '@/components/ui/textarea';
import { notify } from '@/lib/notify.ts';
import { strings } from '@/strings.ts';
import {
  AUTH_KINDS,
  build,
  initialState,
  isAuthKind,
  serverErrorField,
  type FieldErrors,
  type FieldName,
  type FormState,
} from './formModel.ts';

const t = strings.form;

interface FieldProps {
  label: string;
  hint?: string | undefined;
  error?: string | undefined;
  children: (a: {
    id: string;
    invalid: true | undefined;
    describedBy: string | undefined;
  }) => ReactNode;
}

/** Label, control, hint and the error under it (wired with aria-invalid / aria-describedby). */
function Field({ label, hint, error, children }: FieldProps) {
  const id = useId();
  const hintId = `${id}-hint`;
  const errorId = `${id}-error`;
  const describedBy = [error ? errorId : null, hint ? hintId : null].filter(Boolean).join(' ');
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children({
        id,
        invalid: error ? true : undefined,
        describedBy: describedBy || undefined,
      })}
      {error && (
        <p id={errorId} className="text-sm text-destructive">
          {error}
        </p>
      )}
      {hint && (
        <p id={hintId} className="text-sm text-muted-foreground">
          {hint}
        </p>
      )}
    </div>
  );
}

export interface ServerFormProps {
  /** Present when editing; absent when creating. */
  server?: ServerView;
  onSaved: (server: ServerView) => void;
}

/** Create / edit form of an upstream server (auth mode decides the fields shown). */
export function ServerForm({ server, onSaved }: ServerFormProps) {
  const editing = server !== undefined;
  const client = useQueryClient();
  const [f, setF] = useState<FormState>(() => initialState(server));
  const [errors, setErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<unknown>(null);
  const set = <K extends keyof FormState>(key: K, value: FormState[K]) =>
    setF((prev) => ({ ...prev, [key]: value }));

  const save = useMutation({
    mutationFn: async ({
      body,
      secret,
    }: {
      body: Record<string, unknown>;
      secret: string | undefined;
    }) => {
      if (!server) return api.post('/api/servers', body, ServerView);
      const base = `/api/servers/${encodeURIComponent(server.slug)}`;
      const updated = await api.put(base, body, ServerView);
      if (secret !== undefined) await api.put(`${base}/secret`, { secret });
      return updated;
    },
    onSuccess: async (saved) => {
      setF((prev) => ({ ...prev, secret: '' })); // a typed secret is never kept after saving
      await client.invalidateQueries({ queryKey: queryKeys.servers });
      await client.invalidateQueries({ queryKey: queryKeys.server(saved.slug) });
      notify.success(editing ? strings.servers.saved : strings.servers.created);
      onSaved(saved);
    },
    onError: async (e) => {
      if (server) {
        // The server PUT may have succeeded before /secret failed: show the real state.
        await client.invalidateQueries({ queryKey: queryKeys.server(server.slug) });
        await client.invalidateQueries({ queryKey: queryKeys.tools(server.slug) });
      }
      if (isApiError(e)) {
        const mapped = serverErrorField(e.code, e.message);
        if (mapped) {
          setErrors({ [mapped.field]: mapped.text });
          return;
        }
        if (e.code.startsWith('invalid_')) {
          setFormError(e.message);
          return;
        }
      }
      setFormError(e);
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    setFormError(null);
    const built = build(f, editing ? 'edit' : 'create');
    if (!built.ok) {
      setErrors(built.errors);
      return;
    }
    setErrors({});
    save.mutate({ body: built.body, secret: built.secret });
  }

  const err = (name: FieldName) => errors[name];
  const hasSecret = f.kind === 'shared_secret' || f.kind === 'oauth_user';
  const kindChanged = editing && server.auth.kind !== f.kind;
  const secretStored = editing && server.secretSet && !kindChanged;
  const text = (
    name: Exclude<keyof FormState, 'params' | 'kind' | 'accessKind'>,
    label: string,
    opts: { hint?: string; type?: string; disabled?: boolean; maxLength?: number } = {},
  ) => (
    <Field label={label} hint={opts.hint} error={err(name)}>
      {({ id, invalid, describedBy }) => (
        <Input
          id={id}
          type={opts.type ?? 'text'}
          autoComplete="off"
          value={f[name]}
          maxLength={opts.maxLength ?? 2048}
          disabled={opts.disabled}
          aria-invalid={invalid}
          aria-describedby={describedBy}
          onChange={(e) => set(name, e.target.value)}
        />
      )}
    </Field>
  );

  return (
    <form className="flex max-w-2xl flex-col gap-5" noValidate onSubmit={submit}>
      {formError !== null && (
        <ErrorAlert
          title={t.errorTitle}
          {...(typeof formError === 'string' ? { message: formError } : { error: formError })}
        />
      )}
      {text('slug', t.slug, { hint: t.slugHint, disabled: editing, maxLength: 64 })}
      {text('name', t.name, { maxLength: 100 })}
      <Field label={t.description} error={err('description')}>
        {({ id, invalid, describedBy }) => (
          <Textarea
            id={id}
            value={f.description}
            maxLength={500}
            aria-invalid={invalid}
            aria-describedby={describedBy}
            onChange={(e) => set('description', e.target.value)}
          />
        )}
      </Field>
      {text('url', t.url, { hint: t.urlHint })}

      <fieldset className="flex flex-col gap-3">
        <legend className="mb-1 text-sm font-medium">{t.authKind}</legend>
        <RadioGroup
          value={f.kind}
          onValueChange={(v) => isAuthKind(v) && set('kind', v)}
          aria-invalid={err('kind') ? true : undefined}
        >
          {AUTH_KINDS.map((kind) => (
            <div key={kind} className="flex items-center gap-2">
              <RadioGroupItem value={kind} aria-label={strings.servers.authKind[kind]} />
              <span className="text-sm">{strings.servers.authKind[kind]}</span>
            </div>
          ))}
        </RadioGroup>
        {err('kind') && <p className="text-sm text-destructive">{err('kind')}</p>}
        {kindChanged && server.secretSet && (
          <p role="status" className="flex items-start gap-2 text-sm text-destructive">
            <TriangleAlertIcon aria-hidden className="mt-0.5 size-4 shrink-0" />
            {t.secretResetWarning}
          </p>
        )}
      </fieldset>

      {f.kind !== 'none' && (
        <>
          {text('header', t.header, { maxLength: 100 })}
          {text('template', t.template, { hint: t.templateHint, maxLength: 500 })}
        </>
      )}

      {f.kind === 'oauth_user' && (
        <>
          {text('authorizeUrl', t.authorizeUrl)}
          {text('tokenUrl', t.tokenUrl)}
          {text('revokeUrl', t.revokeUrl)}
          {text('clientId', t.clientId, { maxLength: 500 })}
          {text('scopes', t.scopes, { hint: t.scopesHint })}
          <fieldset className="flex flex-col gap-2">
            <legend className="text-sm font-medium">{t.extraParams}</legend>
            <p className="text-sm text-muted-foreground">{t.extraParamsHint}</p>
            {f.params.map((p, i) => (
              <div key={i} className="flex gap-2">
                <Input
                  aria-label={t.paramKey(i + 1)}
                  value={p.key}
                  maxLength={100}
                  onChange={(e) =>
                    set(
                      'params',
                      f.params.map((x, j) => (j === i ? { ...x, key: e.target.value } : x)),
                    )
                  }
                />
                <Input
                  aria-label={t.paramValue(i + 1)}
                  value={p.value}
                  maxLength={1000}
                  onChange={(e) =>
                    set(
                      'params',
                      f.params.map((x, j) => (j === i ? { ...x, value: e.target.value } : x)),
                    )
                  }
                />
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  aria-label={t.removeParam(i + 1)}
                  onClick={() =>
                    set(
                      'params',
                      f.params.filter((_, j) => j !== i),
                    )
                  }
                >
                  <XIcon aria-hidden />
                </Button>
              </div>
            ))}
            <div>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => set('params', [...f.params, { key: '', value: '' }])}
              >
                <PlusIcon aria-hidden />
                {t.addParam}
              </Button>
            </div>
          </fieldset>
        </>
      )}

      {hasSecret && (
        <Field
          label={f.kind === 'oauth_user' ? t.clientSecret : t.sharedSecret}
          hint={secretStored ? t.secretKeepHint : undefined}
          error={err('secret')}
        >
          {({ id, invalid, describedBy }) => (
            <div className="flex items-center gap-2">
              <Input
                id={id}
                type="password"
                autoComplete="new-password"
                value={f.secret}
                maxLength={4096}
                aria-invalid={invalid}
                aria-describedby={describedBy}
                onChange={(e) => set('secret', e.target.value)}
              />
              {secretStored && <Badge variant="secondary">{t.secretSet}</Badge>}
            </div>
          )}
        </Field>
      )}

      <fieldset className="flex flex-col gap-3">
        <legend className="mb-1 text-sm font-medium">{t.access}</legend>
        <RadioGroup
          value={f.accessKind}
          onValueChange={(v) => (v === 'all' || v === 'groups') && set('accessKind', v)}
        >
          <div className="flex items-center gap-2">
            <RadioGroupItem value="all" aria-label={t.accessAll} />
            <span className="text-sm">{t.accessAll}</span>
          </div>
          <div className="flex items-center gap-2">
            <RadioGroupItem value="groups" aria-label={t.accessGroups} />
            <span className="text-sm">{t.accessGroups}</span>
          </div>
        </RadioGroup>
        {f.accessKind === 'groups' && text('groups', t.groups, { hint: t.groupsHint })}
      </fieldset>

      {text('timeoutMs', t.timeoutMs, { type: 'number' })}
      {text('dailyLimit', t.dailyLimit, { type: 'number', hint: t.dailyLimitHint })}

      <div>
        <Button type="submit" disabled={save.isPending}>
          {save.isPending ? strings.common.saving : strings.common.save}
        </Button>
      </div>
    </form>
  );
}
