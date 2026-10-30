import { type CatalogServer } from '@mcp/contracts';
import { LibraryIcon } from 'lucide-react';
import { useState } from 'react';
import { useCatalog } from '@/api/queries.ts';
import { EmptyState } from '@/components/EmptyState.tsx';
import { ErrorAlert } from '@/components/ErrorAlert.tsx';
import { Badge } from '@/components/ui/badge';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import { strings } from '@/strings.ts';

const t = strings.catalog;

function matches(text: string, q: string) {
  return text.toLowerCase().includes(q);
}

function ServerCard({ server, query }: { server: CatalogServer; query: string }) {
  const tools = query
    ? server.tools.filter((x) => matches(`${x.ref} ${x.title ?? ''} ${x.description}`, query))
    : server.tools;
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          {server.name}{' '}
          <span className="font-mono text-xs text-muted-foreground">{server.slug}</span>
        </CardTitle>
        {server.description && <CardDescription>{server.description}</CardDescription>}
        <p className="text-sm text-muted-foreground">{t.tools(server.tools.length)}</p>
      </CardHeader>
      <CardContent>
        {tools.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t.noTools}</p>
        ) : (
          <ul className="flex flex-col gap-3">
            {tools.map((tool) => (
              <li key={tool.ref} className="flex flex-col gap-1">
                <div className="flex flex-wrap items-center gap-2">
                  <code className="text-sm font-medium">{tool.ref}</code>
                  {tool.annotations.readOnlyHint === true && (
                    <Badge variant="secondary">{t.readOnly}</Badge>
                  )}
                  {tool.annotations.destructiveHint === true && (
                    <Badge variant="destructive">{t.destructive}</Badge>
                  )}
                </div>
                {tool.description && (
                  <p className="text-sm text-muted-foreground">{tool.description}</p>
                )}
              </li>
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}

/** `/catalog`: the servers and visible tools this person can reach through the gateway. */
export function CatalogPage() {
  const catalog = useCatalog();
  const [search, setSearch] = useState('');
  const query = search.trim().toLowerCase();

  let body;
  if (catalog.isError) body = <ErrorAlert error={catalog.error} />;
  else if (catalog.data === undefined) body = <Skeleton className="h-32 w-full" />;
  else if (catalog.data.length === 0) {
    body = <EmptyState icon={LibraryIcon} title={t.empty} description={t.emptyHint} />;
  } else {
    const shown = query
      ? catalog.data.filter(
          (s) =>
            matches(`${s.name} ${s.slug} ${s.description}`, query) ||
            s.tools.some((x) => matches(`${x.ref} ${x.title ?? ''} ${x.description}`, query)),
        )
      : catalog.data;
    body =
      shown.length === 0 ? (
        <EmptyState title={t.nothingFound} />
      ) : (
        <div className="flex flex-col gap-4">
          {shown.map((s) => (
            <ServerCard key={s.slug} server={s} query={query} />
          ))}
        </div>
      );
  }

  return (
    <div className="mx-auto flex w-full max-w-4xl flex-col gap-4">
      <h1 className="text-2xl font-semibold">{t.title}</h1>
      <p className="text-sm text-muted-foreground">{t.hint}</p>
      <Input
        type="search"
        aria-label={t.search}
        placeholder={t.search}
        value={search}
        onChange={(e) => setSearch(e.target.value)}
      />
      {body}
    </div>
  );
}
