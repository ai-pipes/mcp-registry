// Package domain holds the pure rules of the search module: the text of a tool that is embedded
// and indexed, the words of its input schema, reciprocal rank fusion with the score, and the
// limits of a query. Plain Go, no I/O.
//
// Lengths and cuts count UTF-16 code units, as the TypeScript server's String.length and slice do.
package domain
