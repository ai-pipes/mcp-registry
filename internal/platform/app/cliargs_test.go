package app

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// parseArgs mirrors node:util parseArgs({strict: true, allowPositionals: true}) as v1.0.1:apps/server/src/platform/cli-commands.ts
// uses it; only the message shape of its own errors is pinned by the compatibility suite.
func TestParseArgs(t *testing.T) {
	spec := argSpec{Strings: []string{"sub", "name"}, Bools: []string{"admin", "member"}}
	for _, tc := range []struct {
		name string
		args []string
		want parsedArgs
	}{
		{"separate values", []string{"--sub", "a", "--name", "B C"}, parsedArgs{values: map[string]string{"sub": "a", "name": "B C"}}},
		{"equals form", []string{"--sub=a", "--name=x=y"}, parsedArgs{values: map[string]string{"sub": "a", "name": "x=y"}}},
		{"an empty value is a value", []string{"--name", ""}, parsedArgs{values: map[string]string{"name": ""}}},
		{"a lone dash is a value", []string{"--name", "-"}, parsedArgs{values: map[string]string{"name": "-"}}},
		{"booleans", []string{"--admin"}, parsedArgs{values: map[string]string{}, flags: map[string]bool{"admin": true}}},
		{"the last repeat wins", []string{"--sub", "a", "--sub", "b"}, parsedArgs{values: map[string]string{"sub": "b"}}},
		{"positionals", []string{"x", "--sub", "a", "y", "-"}, parsedArgs{values: map[string]string{"sub": "a"}, positionals: []string{"x", "y", "-"}}},
		{"everything after -- is positional", []string{"--", "--sub", "-x"}, parsedArgs{values: map[string]string{}, positionals: []string{"--sub", "-x"}}},
		{"a value after the terminator", []string{"--sub", "a", "--", "p"}, parsedArgs{values: map[string]string{"sub": "a"}, positionals: []string{"p"}}},
		{"a value that looks like an option via equals", []string{"--name=-x"}, parsedArgs{values: map[string]string{"name": "-x"}}},
	} {
		got, err := spec.parse(tc.args)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got.flags == nil {
			got.flags = map[string]bool{}
		}
		if tc.want.flags == nil {
			tc.want.flags = map[string]bool{}
		}
		if !reflect.DeepEqual(got.values, tc.want.values) || !reflect.DeepEqual(got.flags, tc.want.flags) || !reflect.DeepEqual(got.positionals, tc.want.positionals) {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// Each expected text is pasted from Node 22.21 (the image's major version):
// util.parseArgs({args, options: {sub: string, name: string, admin: boolean}, strict: true,
// allowPositionals: true}), e.message. v1.0.1:apps/server/src/platform/cli-commands.ts prints `error: ${error.message}`.
func TestParseArgsRefusalsAreTheTextsOfNode(t *testing.T) {
	spec := argSpec{Strings: []string{"sub", "name"}, Bools: []string{"admin"}}
	const unknownTail = ". To specify a positional argument starting with a '-', place it at the end of the command after '--', as in '-- \""
	ambiguous := func(n string) string {
		return "Option '--" + n + "' argument is ambiguous.\nDid you forget to specify the option argument for '--" + n + "'?\nTo specify an option argument starting with a dash use '--" + n + "=-XYZ'."
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"a value that looks like an option", []string{"--sub", "--admin"}, ambiguous("sub")},
		{"a value that looks like a short option", []string{"--sub", "-x"}, ambiguous("sub")},
		{"unknown long option", []string{"--foo"}, "Unknown option '--foo'" + unknownTail + `--foo"`},
		{"unknown long option with a value names only the option", []string{"--foo=bar"}, "Unknown option '--foo'" + unknownTail + `--foo"`},
		{"a short group names its first letter", []string{"-abc"}, "Unknown option '-a'" + unknownTail + `-a"`},
		{"a single short option", []string{"-a"}, "Unknown option '-a'" + unknownTail + `-a"`},
		{"a value missing at the end", []string{"--sub"}, "Option '--sub <value>' argument missing"},
		{"a value for a boolean", []string{"--admin=1"}, "Option '--admin' does not take an argument"},
	} {
		_, err := spec.parse(tc.args)
		var ue *UsageErr
		if !errors.As(err, &ue) || ue.Msg != tc.want {
			t.Errorf("%s: %v\nwant %q", tc.name, err, tc.want)
		}
	}
}

func TestParseArgsCountsPositionalsBeforeRequiredOptions(t *testing.T) {
	spec := argSpec{Strings: []string{"sub"}}
	p, err := spec.parse([]string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	err = p.expectPositionals(1)
	var ue *UsageErr
	if !errors.As(err, &ue) || ue.Msg != "expected 1 argument(s), got 2" {
		t.Errorf("%v", err)
	}
	if err := p.expectPositionals(2); err != nil {
		t.Error(err)
	}
	// --name "" counts as missing.
	p, _ = spec.parse([]string{"--sub", ""})
	if _, err := p.required("sub"); err == nil || err.Error() != "--sub is required" {
		t.Errorf("%v", err)
	}
	p, _ = spec.parse(nil)
	if _, err := p.required("sub"); err == nil || err.Error() != "--sub is required" {
		t.Errorf("%v", err)
	}
}

func TestUsageNamesEveryCommandOfTheReference(t *testing.T) {
	got := usageFor(map[string]CLICommand{
		"create-user":  {Name: "create-user", Usage: "  create-user --x\n"},
		"create-token": {Name: "create-token", Usage: "  create-token --x\n"},
		"login-link":   {Name: "login-link", Usage: "  login-link --x\n"},
		// Registered since milestone 3: their pending entries are gone.
		"create-server": {Name: "create-server", Usage: "  create-server --x\n"},
		"sync":          {Name: "sync", Usage: "  sync --x\n"},
		// Registered since milestone 4.
		"reindex": {Name: "reindex", Usage: "  reindex\n"},
		// Registered since milestone 7.
		"rotate-secrets-key": {Name: "rotate-secrets-key", Usage: "  rotate-secrets-key\n"},
		"reset-key-check":    {Name: "reset-key-check", Usage: "  reset-key-check --yes\n"},
	})
	for _, name := range usageOrder {
		if !strings.Contains(got, name) {
			t.Errorf("the usage does not name %s:\n%s", name, got)
		}
	}
}
