// Package dsnenv resolves the database connection string from the environment.
//
// NOEWRIGHT_DSN is the current name. LOOM_DSN, from before the project was
// renamed from loom, is still read when NOEWRIGHT_DSN is unset, so existing
// shells, CI and scripts keep working. The fallback is temporary; see
// MIGRATING.md.
package dsnenv

import "os"

const (
	// Var is the environment variable that holds the DSN.
	Var = "NOEWRIGHT_DSN"
	// LegacyVar is the pre-rename name, read only when Var is unset.
	LegacyVar = "LOOM_DSN"
)

// Get returns NOEWRIGHT_DSN, or LOOM_DSN when NOEWRIGHT_DSN is unset or empty.
func Get() string {
	if v := os.Getenv(Var); v != "" {
		return v
	}
	return os.Getenv(LegacyVar)
}
