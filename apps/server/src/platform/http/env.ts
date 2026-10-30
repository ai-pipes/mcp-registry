/** Per-request variables shared by the platform middleware and every module's routes. */
export type AppEnv = {
  Variables: {
    requestId: string;
    clientIp: string;
  };
};
