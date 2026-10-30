/** Longest text handed to the embedder or to the full-text body: keeps one call well inside the outbox lease. */
export const MAX_PASSAGE_CHARS = 2000;

export type PassageSource = {
  serverName: string;
  serverDescription: string;
  toolName: string;
  title: string | null;
  description: string;
};

/**
 * Embedding passage: `"<server name>. <server description>\n<tool name> <title>\n<description>"`,
 * truncated to MAX_PASSAGE_CHARS. The server part lets a query name the product ("in the tracker").
 */
export function buildPassage(s: PassageSource): string {
  const head = s.title ? `${s.toolName} ${s.title}` : s.toolName;
  return `${s.serverName}. ${s.serverDescription}\n${head}\n${s.description}`.slice(
    0,
    MAX_PASSAGE_CHARS,
  );
}

const isRecord = (v: unknown): v is Record<string, unknown> =>
  typeof v === 'object' && v !== null && !Array.isArray(v);

function collect(schema: unknown, out: string[]): void {
  if (!isRecord(schema)) return;
  if (typeof schema['description'] === 'string') out.push(schema['description']);
  if (isRecord(schema['properties'])) {
    for (const [name, sub] of Object.entries(schema['properties'])) {
      out.push(name);
      collect(sub, out);
    }
  }
  collect(schema['items'], out);
}

/**
 * Words of an input schema (parameter names and their descriptions, nested ones included):
 * the full-text body of a tool, so a query can match "labels" or "assignee".
 */
export function schemaText(inputSchema: Record<string, unknown>): string {
  const out: string[] = [];
  // The schema's own description repeats the tool's: only the parameters count.
  collect({ properties: inputSchema['properties'], items: inputSchema['items'] }, out);
  return out.join(' ').slice(0, MAX_PASSAGE_CHARS);
}
