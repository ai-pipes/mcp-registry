/** How a server authenticates to upstream (catalog's `AuthMode` kinds). */
type AuthKind = 'none' | 'shared_secret' | 'oauth_user' | 'pat_user';

/** The person's connection to a personal server, as `listConnections` reports it. */
type PersonalStatus = 'ready' | 'expired' | 'missing';

const STATES = ['ready', 'needs_connection', 'expired', 'not_required'] as const;

/** What the agent is told about a server's connection in `list_servers`, `search_tools`, `describe_tool`. */
export type AgentConnection = (typeof STATES)[number];

export const AgentConnection = {
  all: STATES,

  /**
   * `none`/`shared_secret` servers need nothing from the person. Personal servers (`oauth_user`,
   * `pat_user`) are `ready` or `expired` as the connection says; without one, `needs_connection`.
   */
  of(authKind: AuthKind, status: PersonalStatus | undefined): AgentConnection {
    if (authKind === 'none' || authKind === 'shared_secret') return 'not_required';
    if (status === 'ready' || status === 'expired') return status;
    return 'needs_connection';
  },

  /** The person has to act in the UI: the agent gets the link to the connections page. */
  needsLink(connection: AgentConnection): boolean {
    return connection === 'needs_connection' || connection === 'expired';
  },
};
