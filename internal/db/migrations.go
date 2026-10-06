package db

import (
	"embed"
	"io/fs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations is the goose migration set, rooted at the migrations directory.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(err) // the embed pattern guarantees the directory exists
	}
	return sub
}
