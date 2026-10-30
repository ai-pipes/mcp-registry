/** Sent to the client at connect; tells the agent when and how to use the four meta-tools. */
export const SERVER_INSTRUCTIONS =
  "This server is a gateway to the user's services (git hosting, issue tracker and others). When a task needs one of them, call search_tools with a short description of the action, then describe_tool for the chosen tool, then call_tool. If nothing fits, continue without them. If a tool needs a connection, tell the user the link from the error.";
