package db

import "github.com/jackc/pgx/v5/pgtype"

// Numeric converts a decimal string like "0.0025" to a NUMERIC, or NULL when
// it is empty or malformed.
func Numeric(s string) pgtype.Numeric {
	var n pgtype.Numeric
	if s == "" || n.Scan(s) != nil {
		return pgtype.Numeric{}
	}
	return n
}
