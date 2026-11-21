package http

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

// The request bodies are read the way the TypeScript server reads them: JSON.parse into a tree,
// then a zod schema over the tree. A tree rather than a struct decode keeps what zod sees and a
// Go struct would lose: the order of members (extraAuthorizeParams is ordered), duplicate members
// (JSON.parse keeps the last value), `{}` apart from absent, and all the problems of a body at
// once, so that the first one in schema order can be reported.

type nodeKind uint8

const (
	kindNull nodeKind = iota
	kindBool
	kindNumber
	kindString
	kindArray
	kindObject
)

// node is one JSON value. For a number, str is its source text.
type node struct {
	kind  nodeKind
	str   string
	items []*node
	keys  []string // object members in document order, duplicates kept
	vals  []*node
}

// member is the value of an object member: the LAST one when the key is repeated, as JSON.parse
// does; nil when absent.
func (n *node) member(key string) *node {
	for i := len(n.keys) - 1; i >= 0; i-- {
		if n.keys[i] == key {
			return n.vals[i]
		}
	}
	return nil
}

func readNode(dec *jsontext.Decoder) (*node, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	switch tok.Kind() {
	case 'n':
		return &node{kind: kindNull}, nil
	case 't', 'f':
		return &node{kind: kindBool, str: tok.String()}, nil
	case '"':
		return &node{kind: kindString, str: tok.String()}, nil
	case '0':
		return &node{kind: kindNumber, str: tok.String()}, nil
	case '[':
		n := &node{kind: kindArray, items: []*node{}}
		for dec.PeekKind() != ']' {
			item, err := readNode(dec)
			if err != nil {
				return nil, err
			}
			n.items = append(n.items, item)
		}
		_, err := dec.ReadToken()
		return n, err
	case '{':
		n := &node{kind: kindObject}
		for dec.PeekKind() != '}' {
			keyTok, err := dec.ReadToken()
			if err != nil {
				return nil, err
			}
			key := keyTok.String() // a token is valid only until the next read
			val, err := readNode(dec)
			if err != nil {
				return nil, err
			}
			n.keys, n.vals = append(n.keys, key), append(n.vals, val)
		}
		_, err := dec.ReadToken()
		return n, err
	}
	return nil, errors.New("unexpected token")
}

// readTree parses a whole body. Invalid UTF-8 is replaced and duplicate members are kept, as
// JSON.parse does.
func readTree(raw []byte) (*node, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	n, err := readNode(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the value")
	}
	return n, nil
}

// readBody is the body as a tree: an empty (or blank) body is `{}`, a body that is not JSON is
// invalid_request "body is not valid JSON", and one cut by the body limit is the limit's answer.
func readBody(r *http.Request) (*node, error) {
	var raw []byte
	if r.Body != nil {
		var err error
		if raw, err = io.ReadAll(r.Body); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return nil, err
			}
			return nil, httpx.InvalidRequest("body could not be read")
		}
	}
	text := kernel.JSTrim(string(raw))
	if text == "" {
		text = "{}"
	}
	n, err := readTree([]byte(text))
	if err != nil {
		return nil, httpx.InvalidRequest("body is not valid JSON")
	}
	return n, nil
}

// issue is one problem of a body: the dotted path as zod reports it ("" for the whole body).
type issue struct{ path, msg string }

func (i issue) err() error {
	if i.path == "" {
		return httpx.InvalidRequest(i.msg)
	}
	return httpx.InvalidRequest(i.path + ": " + i.msg)
}

func join(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

// shape collects the type and presence problems of a body (the part of a zod schema that is not
// a limit).
type shape struct{ issues []issue }

func (s *shape) add(path, msg string) { s.issues = append(s.issues, issue{path, msg}) }

// object is the member's object, or nil with an issue (absent, null and other types alike).
func (s *shape) object(parent *node, base, key string) *node {
	p := join(base, key)
	n := parent.member(key)
	switch {
	case n == nil:
		s.add(p, "is required")
	case n.kind != kindObject:
		s.add(p, "expected an object")
	default:
		return n
	}
	return nil
}

// text checks a string value; a NUL cannot be stored (Postgres refuses it), so it is a refusal
// here, where the TypeScript server would answer 500.
func (s *shape) text(n *node, path string) (string, bool) {
	switch {
	case n == nil:
		s.add(path, "is required")
	case n.kind != kindString:
		s.add(path, "expected a string")
	case strings.ContainsRune(n.str, 0):
		s.add(path, "must not contain a NUL character")
	default:
		return n.str, true
	}
	return "", false
}

func (s *shape) str(parent *node, base, key string) string {
	v, _ := s.text(parent.member(key), join(base, key))
	return v
}

// optionalStr is z.string().optional(): absent is nil; null is not a string.
func (s *shape) optionalStr(parent *node, base, key string) *string {
	n := parent.member(key)
	if n == nil {
		return nil
	}
	if v, ok := s.text(n, join(base, key)); ok {
		return &v
	}
	return nil
}

// strList is z.array(z.string()): every element is checked; a wrong one is left out.
func (s *shape) strList(n *node, path string) []string {
	out := []string{}
	switch {
	case n == nil:
		s.add(path, "is required")
	case n.kind != kindArray:
		s.add(path, "expected an array")
	default:
		for i, item := range n.items {
			if v, ok := s.text(item, path+"."+strconv.Itoa(i)); ok {
				out = append(out, v)
			}
		}
	}
	return out
}

// maxExactInt is the largest magnitude a JavaScript number holds as an exact integer, far above
// every limit of the contract; larger values are clamped so that the limit check refuses them.
const maxExactInt = 1 << 53

// integer is z.number().int(): a JSON number with an integral value (1000.0 and 1e3 count).
func (s *shape) integer(n *node, path string) (int, bool) {
	if n.kind != kindNumber {
		s.add(path, "expected a number")
		return 0, false
	}
	f, err := strconv.ParseFloat(n.str, 64)
	switch {
	case err != nil && !errors.Is(err, strconv.ErrRange), math.IsInf(f, 0), math.IsNaN(f):
		s.add(path, "expected a finite number")
		return 0, false
	case f != math.Trunc(f):
		s.add(path, "expected an integer")
		return 0, false
	}
	return int(math.Max(-maxExactInt, math.Min(maxExactInt, f))), true
}

func (s *shape) authMode(parent *node) domain.AuthMode {
	none := domain.AuthMode{Kind: domain.AuthNone}
	n := parent.member("auth")
	switch {
	case n == nil:
		s.add("auth", "is required")
		return none
	case n.kind != kindObject:
		s.add("auth", "expected an object")
		return none
	}
	kind := n.member("kind")
	if kind == nil || kind.kind != kindString {
		s.add("auth.kind", "must be one of none, shared_secret, pat_user, oauth_user")
		return none
	}
	mode := domain.AuthMode{Kind: domain.AuthKind(kind.str)}
	switch mode.Kind {
	case domain.AuthNone:
	case domain.AuthSharedSecret, domain.AuthPATUser:
		mode.Header, mode.Template = s.str(n, "auth", "header"), s.str(n, "auth", "template")
	case domain.AuthOAuthUser:
		// The members of the union's branch in schema order: kind, oauth, header, template.
		mode.OAuth = s.oauth(n)
		mode.Header, mode.Template = s.str(n, "auth", "header"), s.str(n, "auth", "template")
	default:
		s.add("auth.kind", "must be one of none, shared_secret, pat_user, oauth_user")
		return none
	}
	return mode
}

func (s *shape) oauth(auth *node) *domain.OAuthClientConfig {
	o := s.object(auth, "auth", "oauth")
	if o == nil {
		return nil
	}
	const base = "auth.oauth"
	cfg := &domain.OAuthClientConfig{
		AuthorizeURL: s.str(o, base, "authorizeUrl"),
		TokenURL:     s.str(o, base, "tokenUrl"),
		RevokeURL:    s.optionalStr(o, base, "revokeUrl"),
		ClientID:     s.str(o, base, "clientId"),
		Scopes:       s.strList(o.member("scopes"), base+".scopes"),
	}
	if extra := o.member("extraAuthorizeParams"); extra != nil {
		cfg.ExtraAuthorizeParams = s.params(extra, base+".extraAuthorizeParams")
	}
	return cfg
}

// params is z.record(z.string(), z.string()) as an ordered list, with the semantics of a
// JavaScript object: a repeated key keeps its FIRST position and its LAST value. `{}` is an
// empty, non-nil list.
func (s *shape) params(n *node, path string) []domain.AuthorizeParam {
	if n.kind != kindObject {
		s.add(path, "expected an object")
		return nil
	}
	out := []domain.AuthorizeParam{}
	at := map[string]int{}
	for i, key := range n.keys {
		value, ok := s.text(n.vals[i], path+"."+key)
		if !ok {
			continue
		}
		if pos, dup := at[key]; dup {
			out[pos].Value = value
			continue
		}
		at[key] = len(out)
		out = append(out, domain.AuthorizeParam{Key: key, Value: value})
	}
	return out
}

func (s *shape) access(parent *node) domain.Access {
	all := domain.Access{Kind: domain.AccessAll}
	n := parent.member("access")
	switch {
	case n == nil:
		s.add("access", "is required")
		return all
	case n.kind != kindObject:
		s.add("access", "expected an object")
		return all
	}
	kind := n.member("kind")
	if kind != nil && kind.kind == kindString {
		switch domain.AccessKind(kind.str) {
		case domain.AccessAll:
			return all
		case domain.AccessGroups:
			return domain.Access{Kind: domain.AccessGroups, Groups: s.strList(n.member("groups"), "access.groups")}
		}
	}
	s.add("access.kind", "must be one of all, groups")
	return all
}

// fields is the editable part of a server (ServerFields of the contract).
func (s *shape) fields(root *node) app.ServerFields {
	f := app.ServerFields{
		Name:        s.str(root, "", "name"),
		Description: s.str(root, "", "description"),
		URL:         s.str(root, "", "url"),
		Auth:        s.authMode(root),
		Access:      s.access(root),
	}
	// An absent timeout is the default; null is not a number. An absent or null limit is none.
	if n := root.member("timeoutMs"); n != nil {
		if v, ok := s.integer(n, "timeoutMs"); ok {
			f.TimeoutMs = &v
		}
	}
	if n := root.member("dailyLimit"); n != nil && n.kind != kindNull {
		if v, ok := s.integer(n, "dailyLimit"); ok {
			f.DailyLimit = &v
		}
	}
	return f
}

// Where a path stands in the schema, for ordering issues the way zod reports them (object
// members in schema order, array elements before the array's own check).
var (
	rootOrder   = []string{"name", "description", "url", "auth", "access", "timeoutMs", "dailyLimit", "slug", "secret"}
	authOrder   = []string{"kind", "oauth", "header", "template"}
	oauthOrder  = []string{"authorizeUrl", "tokenUrl", "revokeUrl", "clientId", "scopes", "extraAuthorizeParams"}
	accessOrder = []string{"kind", "groups"}
)

func indexOf(order []string, seg string) int {
	for i, s := range order {
		if s == seg {
			return i
		}
	}
	return len(order)
}

func rankOf(path string) []int {
	if path == "" {
		return nil
	}
	var rank []int
	scope := "root"
	segs := strings.Split(path, ".")
	for i := 0; i < len(segs); i++ {
		seg := segs[i]
		switch scope {
		case "root":
			rank = append(rank, indexOf(rootOrder, seg))
			if seg == "auth" || seg == "access" {
				scope = seg
			} else {
				scope = "leaf"
			}
		case "auth":
			rank = append(rank, indexOf(authOrder, seg))
			scope = "leaf"
			if seg == "oauth" {
				scope = "oauth"
			}
		case "oauth":
			rank = append(rank, indexOf(oauthOrder, seg))
			scope = "leaf"
			switch seg {
			case "scopes":
				scope = "list"
			case "extraAuthorizeParams":
				// The rest is a key, which may itself hold dots; keys keep the order they were found in.
				return append(rank, 0)
			}
		case "access":
			rank = append(rank, indexOf(accessOrder, seg))
			scope = "leaf"
			if seg == "groups" {
				scope = "list"
			}
		case "list":
			n, _ := strconv.Atoi(seg)
			rank = append(rank, n)
			scope = "leaf"
		default:
			rank = append(rank, 0)
		}
	}
	return rank
}

// before reports whether rank a comes before rank b. A path that is the parent of another comes
// after its children (the length check of an array follows the checks of its elements).
func before(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) > len(b)
}

// firstIssue is the first problem of a body in schema order, or nil. The shape problems come with
// the limit problems of the application layer for the same values; a limit problem on a value
// that did not have the right shape is dropped (zod says one thing about a value).
func firstIssue(shapeIssues []issue, limits []app.Issue) *issue {
	all := append([]issue{}, shapeIssues...)
next:
	for _, l := range limits {
		for _, s := range shapeIssues {
			if l.Path == s.path || strings.HasPrefix(l.Path, s.path+".") {
				continue next
			}
		}
		all = append(all, issue{l.Path, l.Message})
	}
	if len(all) == 0 {
		return nil
	}
	first := all[0]
	for _, c := range all[1:] {
		if before(rankOf(c.path), rankOf(first.path)) {
			first = c
		}
	}
	return &first
}

// serverRequest is the body of POST (create true: slug and secret too) or PUT /api/servers.
func serverRequest(r *http.Request, create bool) (app.ServerInput, *string, error) {
	root, err := readBody(r)
	if err != nil {
		return app.ServerInput{}, nil, err
	}
	if root.kind != kindObject {
		return app.ServerInput{}, nil, issue{"", "expected an object"}.err()
	}
	var s shape
	in := app.ServerInput{Fields: s.fields(root)}
	var secret *string
	if create {
		in.Slug = s.str(root, "", "slug")
		if n := root.member("secret"); n != nil && n.kind != kindNull {
			if v, ok := s.text(n, "secret"); ok {
				secret = &v
			}
		}
	}
	var limits []app.Issue
	if create {
		limits = app.ValidateCreateRequest(in, secret)
	} else {
		limits = app.ValidateServerFields(in.Fields)
	}
	if first := firstIssue(s.issues, limits); first != nil {
		return app.ServerInput{}, nil, first.err()
	}
	return in, secret, nil
}

// objectBody is a body that must be a JSON object.
func objectBody(r *http.Request) (*node, error) {
	root, err := readBody(r)
	if err != nil {
		return nil, err
	}
	if root.kind != kindObject {
		return nil, issue{"", "expected an object"}.err()
	}
	return root, nil
}

// secretRequest is {"secret": string | null}; null clears. The length is the use case's to check.
func secretRequest(r *http.Request) (*string, error) {
	root, err := objectBody(r)
	if err != nil {
		return nil, err
	}
	n := root.member("secret")
	if n != nil && n.kind == kindNull {
		return nil, nil
	}
	var s shape
	v, ok := s.text(n, "secret")
	if !ok {
		return nil, s.issues[0].err()
	}
	return &v, nil
}

// statusRequest is {"status": "active" | "disabled"}.
func statusRequest(r *http.Request) (domain.ServerStatus, error) {
	root, err := objectBody(r)
	if err != nil {
		return "", err
	}
	if n := root.member("status"); n != nil && n.kind == kindString {
		switch status := domain.ServerStatus(n.str); status {
		case domain.StatusActive, domain.StatusDisabled:
			return status, nil
		}
	}
	return "", issue{"status", "must be active or disabled"}.err()
}

// hiddenRequest is {"hidden": boolean}.
func hiddenRequest(r *http.Request) (bool, error) {
	root, err := objectBody(r)
	if err != nil {
		return false, err
	}
	n := root.member("hidden")
	switch {
	case n == nil:
		return false, issue{"hidden", "is required"}.err()
	case n.kind != kindBool:
		return false, issue{"hidden", "expected a boolean"}.err()
	}
	return n.str == "true", nil
}
