// Command 8c2-hashdigest prints, for each user in an api.db opened
// read-only, its ID, username, role, disabled flag and the SHA-256 of its
// stored password hash: never the hash. The step 8c-2 lab runs it on
// blazanov (as root, through sudo) to show that an upgrade keeps every
// user and hash.
package main

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/url"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: 8c2-hashdigest API.DB")
		os.Exit(2)
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: os.Args[1]}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, username, role_id, disabled, password_hash FROM users ORDER BY username`)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, role, hash string
		var disabled int
		if err := rows.Scan(&id, &name, &role, &disabled, &hash); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("%s %s %s disabled=%d hash-sha256=%x\n", id, name, role, disabled, sha256.Sum256([]byte(hash)))
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
