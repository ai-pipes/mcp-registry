const MAX_DESCRIPTION = 200;
const ELLIPSIS = '…';

/**
 * A tool description for `search_tools`: whitespace collapsed, at most 200 code points. A longer
 * one is cut at the last word boundary (trailing punctuation dropped) and ends with «…»; a single
 * word longer than the limit is cut hard. `describe_tool` returns the full text.
 */
export function shortDescription(description: string): string {
  // Code points, not UTF-16 units: a cut never splits a surrogate pair (emoji and the like).
  const chars = Array.from(description.replace(/\s+/g, ' ').trim());
  if (chars.length <= MAX_DESCRIPTION) return chars.join('');
  const room = MAX_DESCRIPTION - ELLIPSIS.length;
  let cut = chars.slice(0, room).join('');
  const space = cut.lastIndexOf(' ');
  if (chars[room] !== ' ' && space > 0) cut = cut.slice(0, space);
  const trimmed = cut.replace(/[\s,.;:!?—–-]+$/u, '');
  return `${trimmed.length > 0 ? trimmed : cut}${ELLIPSIS}`;
}
