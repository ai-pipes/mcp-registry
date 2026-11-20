package config

import "strings"

// Issue is one invalid variable: its name and why. Message never contains the value.
type Issue struct{ Name, Message string }

// Error is the startup refusal: every bad variable, in declaration order.
type Error struct{ Issues []Issue }

// Error renders "Invalid configuration:" and one "  NAME: message" line per variable.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("Invalid configuration:")
	for _, i := range e.Issues {
		b.WriteString("\n  ")
		b.WriteString(i.Name)
		b.WriteString(": ")
		b.WriteString(i.Message)
	}
	return b.String()
}

// issues collects the problems of one Load in the order the variables are declared.
type issues struct{ list []Issue }

func (c *issues) add(name, message string) { c.list = append(c.list, Issue{name, message}) }
