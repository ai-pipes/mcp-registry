import type { AdminTool } from '@mcp/contracts';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '@/api/client.ts';
import { queryKeys } from '@/api/queries.ts';
import { ErrorAlert } from '@/components/ErrorAlert.tsx';
import { Checkbox } from '@/components/ui/checkbox';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { notify } from '@/lib/notify.ts';
import { strings } from '@/strings.ts';

const t = strings.tools;

function useSetHidden(slug: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ name, hidden }: { name: string; hidden: boolean }) =>
      api.put(`/api/servers/${encodeURIComponent(slug)}/tools/${encodeURIComponent(name)}/hidden`, {
        hidden,
      }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.tools(slug) });
      notify.success(t.hiddenSaved);
    },
  });
}

/** Tools of a server as the admin sees them (hidden ones included), with the "Hidden" switch. */
export function ToolsTable({ slug, tools }: { slug: string; tools: AdminTool[] }) {
  const setHidden = useSetHidden(slug);
  return (
    <div className="flex flex-col gap-2">
      {setHidden.isError && <ErrorAlert error={setHidden.error} />}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t.columns.name}</TableHead>
            <TableHead>{t.columns.description}</TableHead>
            <TableHead>{t.columns.hidden}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {tools.map((tool) => (
            <TableRow key={tool.name} className={tool.hidden ? 'opacity-60' : undefined}>
              <TableCell className="font-mono text-xs">{tool.name}</TableCell>
              <TableCell className="whitespace-normal text-sm text-muted-foreground">
                {tool.description}
              </TableCell>
              <TableCell>
                <Checkbox
                  aria-label={t.hiddenAria(tool.name)}
                  checked={tool.hidden}
                  disabled={setHidden.isPending}
                  onCheckedChange={(checked) =>
                    setHidden.mutate({ name: tool.name, hidden: checked })
                  }
                />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
