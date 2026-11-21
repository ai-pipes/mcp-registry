package app

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// parseArgs mirrors node:util parseArgs({strict: true, allowPositionals: true}) as cli-commands.ts
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

func TestParseArgsRefusalsAreUsageErrors(t *testing.T) {
	spec := argSpec{Strings: []string{"sub"}, Bools: []string{"admin"}}
	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		{"unknown long option", "Unknown option '--bogus'", []string{"--bogus"}},
		{"unknown short option", "Unknown option '-x'", []string{"-x"}},
		{"a value missing at the end", "Option '--sub <value>' argument missing", []string{"--sub"}},
		{"a value that is an option", "Option '--sub <value>' argument is ambiguous", []string{"--sub", "--admin"}},
		{"a value for a boolean", "Option '--admin' does not take an argument", []string{"--admin=1"}},
	} {
		_, err := spec.parse(tc.args)
		var ue *UsageErr
		if !errors.As(err, &ue) || !strings.HasPrefix(ue.Msg, tc.want) {
			t.Errorf("%s: %v, want a usage error starting %q", tc.name, err, tc.want)
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

func TestUsageNamesEveryCommandOfTheReferenceBeforeTheirMilestones(t *testing.T) {
	got := usageFor(map[string]CLICommand{
		"create-user":  {Name: "create-user", Usage: "  create-user --x\n"},
		"create-token": {Name: "create-token", Usage: "  create-token --x\n"},
		"login-link":   {Name: "login-link", Usage: "  login-link --x\n"},
	})
	for _, name := range usageOrder {
		if !strings.Contains(got, name) {
			t.Errorf("the usage does not name %s:\n%s", name, got)
		}
	}
}
