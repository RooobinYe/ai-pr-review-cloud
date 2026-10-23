// Package store persists review runs in PostgreSQL. It owns transactional
// invariants only; it never calls GitHub, Kubernetes, or the model proxy.
package store

import "embed"

// Migrations holds the ordered schema migrations, applied by the controller at startup.
//
//go:embed migrations/*.sql
var Migrations embed.FS
