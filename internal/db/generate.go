package db

// sqlc is run via `go run` at a pinned version instead of being a module
// dependency, so its large dependency tree stays out of go.mod. v1.30.0 is
// the newest release that builds with Go 1.25. Needs cgo (Postgres parser).
//go:generate go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate -f ../../sqlc.yaml
