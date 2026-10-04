// Package filegate provides a portable backend client for the Filegate HTTP API.
// Client and Root expose authenticated operations and direct transfer leases;
// stable-ID reads target an object within an indexed managed root. Raw methods
// preserve non-success HTTP responses for callers to inspect.
package filegate
