//go:build compat

package harness

// REST endpoint -> response contract. A scenario passes the contract name its endpoint returns
// to Response.JSON. Derived from the route files under v1.0.1:apps/server/src/modules/*/interfaces/http/
// and v1.0.1:apps/server/src/platform/http/ (the zod schema each handler parses or types its body with).
// Every non-2xx JSON body is ErrorBody. A name ending in "[]" validates a JSON array whose items
// match the named contract: the contracts package has no list schema for those three endpoints
// (the handlers return a bare array typed by the item schema).
//
//	GET    /api/auth/methods                 200 AuthMethods
//	POST   /api/auth/link                    204 no body (errors: ErrorBody)
//	GET    /api/auth/login                   302 redirect, no body
//	GET    /api/auth/callback                302 redirect, no body
//	POST   /api/auth/logout                  204 no body
//	GET    /api/auth/me                      200 MeResponse
//	GET    /api/version                      200 VersionResponse
//	GET    /api/tokens                       200 TokenSummary[]
//	POST   /api/tokens                       201 CreateTokenResponse
//	DELETE /api/tokens/:id                   204 no body
//	GET    /api/users                        200 UserSummary[]
//	POST   /api/users/:id/disabled           204 no body
//	GET    /api/connections                  200 ConnectionSummary[]
//	POST   /api/connections/:server/start    200 StartConnectResponse
//	GET    /api/connections/callback         302 redirect, no body
//	PUT    /api/connections/:server/pat      204 no body
//	DELETE /api/connections/:server          204 no body
//	GET    /api/catalog                      200 CatalogResponse
//	GET    /api/servers                      200 ServerList
//	POST   /api/servers                      201 ServerView
//	GET    /api/servers/:slug                200 ServerView
//	PUT    /api/servers/:slug                200 ServerView
//	PUT    /api/servers/:slug/secret         204 no body
//	POST   /api/servers/:slug/status         204 no body
//	POST   /api/servers/:slug/sync           200 SyncResult
//	GET    /api/servers/:slug/tools          200 AdminToolList
//	PUT    /api/servers/:slug/tools/:name/hidden  204 no body
//	GET    /api/calls                        200 CallList
//	GET    /api/audit                        200 AuditList
//	GET    /healthz                          200 text/plain "ok" (503 "fail"), not JSON

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func init() { validateSchema = ValidateSchema }

type compiled struct {
	schema *jsonschema.Schema
	err    error
}

var (
	schemaMu    sync.Mutex
	schemaCache = map[string]compiled{}
)

func schemasDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("cannot locate the harness sources")
	}
	return filepath.Join(filepath.Dir(file), "..", "schemas")
}

func loadSchema(name string) (*jsonschema.Schema, error) {
	schemaMu.Lock()
	defer schemaMu.Unlock()
	if c, ok := schemaCache[name]; ok {
		return c.schema, c.err
	}
	s, err := compileSchema(name)
	schemaCache[name] = compiled{s, err}
	return s, err
}

// httpURLFormat replaces the library's "uri" format: every "format": "uri" in compat/schemas comes
// from the contracts' HttpUrl (z.url() refined to http(s) only), so an absolute http or https URL
// with a host is the rule.
var httpURLFormat = &jsonschema.Format{
	Name: "uri",
	Validate: func(v any) error {
		str, ok := v.(string)
		if !ok {
			return nil
		}
		u, err := url.Parse(str)
		if err != nil {
			return err
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("%q is not an http(s) URL", str)
		}
		if u.Host == "" {
			return fmt.Errorf("%q has no host", str)
		}
		return nil
	},
}

func compileSchema(name string) (*jsonschema.Schema, error) {
	if name == "" || strings.ContainsAny(name, `/\.`) {
		return nil, fmt.Errorf("invalid schema name %q", name)
	}
	file := filepath.Join(schemasDir(), name+".json")
	raw, err := os.ReadFile(file) //nolint:gosec // G304: the name is checked above and the directory is fixed
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("unknown schema %q (no %s; run make compat-schemas)", name, file)
		}
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("schema %s: %w", name, err)
	}
	id := "https://compat.invalid/" + name + ".json"
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	c.RegisterFormat(httpURLFormat)
	if err := c.AddResource(id, doc); err != nil {
		return nil, fmt.Errorf("schema %s: %w", name, err)
	}
	s, err := c.Compile(id)
	if err != nil {
		return nil, fmt.Errorf("schema %s: %w", name, err)
	}
	return s, nil
}

// ValidateSchema checks a JSON body against compat/schemas/<name>.json (generated from the zod
// contracts by make compat-schemas). A name ending in "[]" means a JSON array of that contract.
// An unknown name, invalid JSON or any mismatch is an error naming the schema and the JSON path.
//
// What is enforced:
//   - Structure: types, required, enums, min/max, array items.
//   - Extra members: zod emits "additionalProperties": false for every plain z.object (output
//     side), so a field the contract does not declare (a "secret" in a server view) is rejected.
//     z.looseObject and z.record schemas are emitted without it and stay open. The files are used
//     unmodified.
//   - Formats are asserted (the library ignores them otherwise): date-time and uuid with the
//     library's checkers; "uri" with the http(s)-only rule above, which reproduces the contracts'
//     HttpUrl refinement (all 24 "uri" occurrences come from it, so ftp://x is rejected).
//
// What is not enforced: zod .refine/.superRefine/.transform rules are lost in the JSON Schema
// conversion. The only one reproduced is the http(s) URL rule above; other refinements (for
// example anything custom on a request body) are not checked, and neither are zod's stricter WHATWG
// URL parsing or string trimming.
func ValidateSchema(name string, body []byte) error {
	base, isList := strings.CutSuffix(name, "[]")
	s, err := loadSchema(base)
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("schema %s: body is not JSON: %w", name, err)
	}
	if !isList {
		return explain(name, "", s.Validate(doc))
	}
	items, ok := doc.([]any)
	if !ok {
		return fmt.Errorf("schema %s: body is not a JSON array", name)
	}
	for i, item := range items {
		if err := explain(name, fmt.Sprintf("/%d", i), s.Validate(item)); err != nil {
			return err
		}
	}
	return nil
}

// explain turns a validation failure into one error with the schema name and every failing path.
func explain(name, prefix string, err error) error {
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return fmt.Errorf("schema %s: %w", name, err)
	}
	var parts []string
	for _, u := range ve.BasicOutput().Errors {
		if u.Error == nil {
			continue
		}
		path := prefix + u.InstanceLocation
		if path == "" {
			path = "/"
		}
		parts = append(parts, fmt.Sprintf("at %s: %s", path, u.Error))
		if len(parts) == 5 {
			break
		}
	}
	if len(parts) == 0 {
		return fmt.Errorf("schema %s: %w", name, err)
	}
	return fmt.Errorf("schema %s: %s", name, strings.Join(parts, "; "))
}
