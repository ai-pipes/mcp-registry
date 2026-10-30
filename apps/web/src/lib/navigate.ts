/** Full-page navigation to an external URL (the OAuth authorize page); a seam for tests. */
export function redirectTo(url: string): void {
  if (!/^https?:\/\//i.test(url)) throw new Error('refusing to redirect to a non-http(s) URL');
  window.location.assign(url);
}
