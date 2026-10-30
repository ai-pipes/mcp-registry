import { z } from 'zod';

export const ToolAnnotations = z.object({
  readOnlyHint: z.boolean().optional(),
  destructiveHint: z.boolean().optional(),
  idempotentHint: z.boolean().optional(),
  openWorldHint: z.boolean().optional(),
});

/** A tool as a member sees it in the catalog: no schemas, no server config. */
export const CatalogTool = z.object({
  ref: z.string(),
  title: z.string().nullable(),
  description: z.string(),
  annotations: ToolAnnotations,
});
export type CatalogTool = z.infer<typeof CatalogTool>;

export const CatalogServer = z.object({
  slug: z.string(),
  name: z.string(),
  description: z.string(),
  tools: z.array(CatalogTool),
});
export type CatalogServer = z.infer<typeof CatalogServer>;

/** `GET /api/catalog` */
export const CatalogResponse = z.array(CatalogServer);
export type CatalogResponse = z.infer<typeof CatalogResponse>;

/** Admin view of a tool: hidden ones are listed too. */
export const AdminTool = CatalogTool.extend({
  name: z.string(),
  inputSchema: z.record(z.string(), z.unknown()),
  outputSchema: z.record(z.string(), z.unknown()).nullable(),
  hidden: z.boolean(),
});
export type AdminTool = z.infer<typeof AdminTool>;

export const AdminToolList = z.array(AdminTool);

/** `PUT /api/servers/:slug/tools/:name/hidden` */
export const SetHiddenRequest = z.object({ hidden: z.boolean() });
export type SetHiddenRequest = z.infer<typeof SetHiddenRequest>;
