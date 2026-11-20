package config

import "strings"

// fromFile supports NAME_FILE: the value of NAME is read from the file the variable points to and
// trimmed. It returns ok=false when NAME_FILE is unset (no issue) or when it is unusable (an issue
// named after NAME_FILE with the path, never the contents).
func fromFile(env map[string]string, name string, readFile func(string) (string, error), c *issues) (value string, ok bool) {
	fileVar := name + "_FILE"
	path, set := env[fileVar]
	if !set {
		return "", false
	}
	if _, both := env[name]; both {
		c.add(fileVar, name+" and "+fileVar+" are mutually exclusive; set only one")
		return "", false
	}
	text, err := readFile(path)
	if err != nil {
		c.add(fileVar, "cannot read the file "+path)
		return "", false
	}
	value = strings.TrimSpace(text)
	if value == "" {
		c.add(fileVar, "the file "+path+" is empty")
		return "", false
	}
	return value, true
}
