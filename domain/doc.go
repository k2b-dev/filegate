// Package domain implements root-scoped file operations and version history.
// Root coordinates atomic publication, durable identities and recovery through
// the Files and State ports. Public identity operations require indexed roots
// managed exclusively through Filegate; indexed unmanaged roots retain the
// internal identities required by their version histories.
package domain
