// Package http holds the REST routes of the catalog module: the catalog every signed-in person
// browses, and the administration of servers and their tools. Sign-in checks come from the
// identity module; nothing here decides who may do what beyond them (the use cases check the role
// again).
package http
