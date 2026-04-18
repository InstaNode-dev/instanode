//go:build ignore

package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	_ "github.com/lib/pq"
)

func main() {
	db, err := sql.Open("postgres", "postgres://postgres:***REDACTED-DROPLET-PG-ROOT***@161.35.111.84:5432/postgres?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	var nolname string
	var nolsuper, nolcreaterole, nolcreatedb bool
	err = db.QueryRowContext(context.Background(), "SELECT rolname, rolsuper, rolcreaterole, rolcreatedb FROM pg_roles WHERE rolname = 'doadmin'").Scan(&nolname, &nolsuper, &nolcreaterole, &nolcreatedb)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Role: %s, Super: %t, CreateRole: %t, CreateDB: %t\n", nolname, nolsuper, nolcreaterole, nolcreatedb)
}
