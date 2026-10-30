import { err, ok, type Result } from '../../../shared-kernel/index.ts';
import { CatalogError } from './errors.ts';
import { ServerSlug } from './server-slug.ts';

/** The tool's name for an agent: `<server>.<tool>`, split on the FIRST dot (upstream names may contain dots). */
export const ToolRef = {
  parse(raw: string): Result<{ server: string; tool: string }, CatalogError> {
    const dot = raw.indexOf('.');
    const server = raw.slice(0, dot);
    const tool = raw.slice(dot + 1);
    if (dot < 1 || tool.length === 0 || !ServerSlug.parse(server).ok) {
      return err(new CatalogError('invalid_tool_ref', 'tool name must look like <server>.<tool>'));
    }
    return ok({ server, tool });
  },
  format(server: string, tool: string): string {
    return `${server}.${tool}`;
  },
};
