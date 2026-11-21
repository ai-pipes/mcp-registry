package app

import (
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// argSpec is the options of a command, parsed like node:util parseArgs({strict: true,
// allowPositionals: true}) in cli-commands.ts: long options only, `--name value` or `--name=value`
// for a string, `--flag` for a boolean, `--` ends the options, a lone `-` is a positional.
type argSpec struct {
	Strings []string
	Bools   []string
}

// parsedArgs is what parse found. A repeated option keeps its last value.
type parsedArgs struct {
	values      map[string]string
	flags       map[string]bool
	positionals []string
}

func isOptionLike(arg string) bool { return len(arg) > 1 && strings.HasPrefix(arg, "-") }

// parse refuses what Node's strict parser refuses, as a UsageError. Only the shape of those
// messages is pinned by the compatibility suite; the wording follows Node's.
func (s argSpec) parse(args []string) (parsedArgs, error) {
	p := parsedArgs{values: map[string]string{}, flags: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			p.positionals = append(p.positionals, args[i+1:]...)
			return p, nil
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg[2:], "=")
			switch {
			case slices.Contains(s.Strings, name):
				if !hasValue {
					rest := args[i+1:]
					if len(rest) == 0 {
						return p, UsageError("Option '--" + name + " <value>' argument missing")
					}
					if isOptionLike(rest[0]) {
						return p, UsageError("Option '--" + name + "' argument is ambiguous.\n" +
							"Did you forget to specify the option argument for '--" + name + "'?\n" +
							"To specify an option argument starting with a dash use '--" + name + "=-XYZ'.")
					}
					value = rest[0]
					i++
				}
				p.values[name] = value
			case slices.Contains(s.Bools, name):
				if hasValue {
					return p, UsageError("Option '--" + name + "' does not take an argument")
				}
				p.flags[name] = true
			default:
				return p, unknownOption(arg)
			}
		case isOptionLike(arg):
			return p, unknownOption(arg)
		default:
			p.positionals = append(p.positionals, arg)
		}
	}
	return p, nil
}

// unknownOption is Node's ERR_PARSE_ARGS_UNKNOWN_OPTION text, byte for byte: it names only the
// option (`--foo` of `--foo=bar`, `-a` of `-abc`) and ends without a closing single quote.
func unknownOption(arg string) error {
	name := arg
	if strings.HasPrefix(arg, "--") {
		name, _, _ = strings.Cut(arg, "=")
	} else {
		_, size := utf8.DecodeRuneInString(arg[1:])
		name = arg[:1+size]
	}
	return UsageError("Unknown option '" + name + "'. To specify a positional argument starting with a '-', " +
		"place it at the end of the command after '--', as in '-- \"" + name + "\"")
}

// expectPositionals is the count check of parse() in cli-commands.ts.
func (p parsedArgs) expectPositionals(n int) error {
	if len(p.positionals) != n {
		return UsageError("expected " + strconv.Itoa(n) + " argument(s), got " + strconv.Itoa(len(p.positionals)))
	}
	return nil
}

// required is a string option that is present and not empty (`--name ""` counts as missing).
func (p parsedArgs) required(name string) (string, error) {
	if v := p.values[name]; v != "" {
		return v, nil
	}
	return "", UsageError("--" + name + " is required")
}
