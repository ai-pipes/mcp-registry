# Validation goldens

Each file records one refused request of the server-administration API, produced by
`TestCatalogValidation` (`compat/catalog_test.go`) against the reference image:

```json
{ "status": 422, "code": "invalid_request", "path": "auth.oauth.clientId", "message": null }
```

- `status`, `code`: always compared exactly.
- `path`: for a body error that zod produces (`code` is `invalid_request`), the dotted field path the
  message starts with (`"slug: Too small: ..."` gives `slug`); empty when the message names none
  (the whole body is wrong). The web form reads it through `serverErrorField` in
  `apps/web/src/admin/servers/formModel.ts`.
- `message`: `null` means "not compared". It is null for zod errors: the wording and the choice of
  the first issue are zod v4's and concern inputs the web form already prevents client-side, so a
  server need not copy them byte for byte.
- Every other refusal keeps its exact message in `message`: the domain errors the form maps to
  fields or shows verbatim (`invalid_slug`, `invalid_auth`, `server_exists`, `server_conflict`, any
  other `invalid_*`) and the one `invalid_request` that is not zod's, `body is not valid JSON`.

Regenerate with `COMPAT_REFERENCE=1 go test -tags compat -run TestCatalogValidation ./compat/ -update`
(delete the directory contents first so that a renamed case leaves no stale file).
