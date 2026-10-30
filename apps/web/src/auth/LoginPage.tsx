import { useSearchParams } from 'react-router';
import { ErrorAlert } from '@/components/ErrorAlert.tsx';
import { buttonVariants } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { useSystemTheme } from '@/lib/useSystemTheme.ts';
import { strings } from '@/strings.ts';

const DEFAULT_RETURN = '/catalog';

/**
 * Only an in-app path may be a return target: one leading `/`, no `//host`, no backslash or
 * control characters (browsers strip tabs/newlines, turning `/\t/host` into `//host`).
 */
function safeReturnTo(path: string): string {
  if (!path.startsWith('/') || path.startsWith('//')) return DEFAULT_RETURN;
  // oxlint-disable-next-line no-control-regex -- control characters are exactly what we reject
  if (/[\\\u0000-\u001f\u007f]/.test(path)) return DEFAULT_RETURN;
  return path === '/' ? DEFAULT_RETURN : path;
}

/**
 * Shown instead of any page for a signed-out visitor. The only way in is the OIDC provider: the button
 * is a plain link to `/api/auth/login`, which redirects there and brings the visitor back to
 * `returnTo`. A failed login comes back as `/?loginError=<code>` (a fixed set of codes).
 */
export function LoginPage({ returnTo }: { returnTo: string }) {
  useSystemTheme();
  const [params] = useSearchParams();
  const code = params.get('loginError');
  const href = `/api/auth/login?returnTo=${encodeURIComponent(safeReturnTo(returnTo))}`;
  return (
    <main className="flex min-h-svh items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>
            <h1 className="text-xl font-semibold">{strings.login.title}</h1>
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          {code !== null && (
            <ErrorAlert
              title={strings.login.errorTitle}
              message={
                Object.hasOwn(strings.login.errors, code)
                  ? strings.login.errors[code]
                  : strings.login.errors.unknown
              }
            />
          )}
          <p className="text-sm text-muted-foreground">{strings.login.hint}</p>
          <a href={href} className={buttonVariants()}>
            {strings.login.submit}
          </a>
        </CardContent>
      </Card>
    </main>
  );
}
