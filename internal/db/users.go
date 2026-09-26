package db

import (
	"database/sql"
	"errors"
)

func (d *DB) UserCount() (int, error) {
	var n int
	err := d.conn.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateFirstUser inserts the user only if the table is empty, atomically, so
// two concurrent setup requests can't both create an account. It reports
// whether the user was created.
func (d *DB) CreateFirstUser(username, passwordHash string) (bool, error) {
	res, err := d.conn.Exec(
		`INSERT INTO users (username, password_hash) SELECT ?, ? WHERE NOT EXISTS (SELECT 1 FROM users)`,
		username, passwordHash,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// PasswordHash returns the stored hash for username, or "" if there is no such user.
func (d *DB) PasswordHash(username string) (string, error) {
	var h string
	err := d.conn.QueryRow(`SELECT password_hash FROM users WHERE username = ?`, username).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return h, err
}
