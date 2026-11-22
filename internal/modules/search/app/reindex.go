package app

import (
	"context"
	"slices"
)

// ReindexAll rebuilds the index from the catalog and drops the documents of vanished or disabled
// servers: IndexTools for every server the catalog lists (disabled ones included) and every
// server that still has documents, one after the other. It returns the number of rows of the whole
// table afterwards (documents of every server, not only the ones just indexed). An embedder
// failure (or any other error of one server's indexing) does not stop the run: the other servers
// are still indexed, then the first error is returned. A cancelled context stops it at once.
func ReindexAll(ctx context.Context, d Deps) (int, error) {
	slugs, err := d.Tools.ListServerSlugs(ctx)
	if err != nil {
		return 0, err
	}
	indexed, err := d.Index.Servers(ctx)
	if err != nil {
		return 0, err
	}
	servers := slices.Clone(slugs)
	for _, s := range indexed {
		if !slices.Contains(servers, s) {
			servers = append(servers, s)
		}
	}
	var first error
	for _, server := range servers {
		if err := IndexTools(ctx, d, server); err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			if first == nil {
				first = err
			}
		}
	}
	if first != nil {
		return 0, first
	}
	return d.Index.Count(ctx)
}
