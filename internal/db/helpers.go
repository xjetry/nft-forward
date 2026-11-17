package db

import "database/sql"

// queryAll executes a query and collects results using the provided scan
// function. It replaces the repeated rows.Next() / scan / append loop that
// appeared in every list function.
func queryAll[T any](d DBTX, query string, scan func(rowScanner) (*T, error), args ...any) ([]*T, error) {
	rows, err := d.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*T
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// count executes a COUNT(*) query and returns the scalar result.
// UpdateRow runs an UPDATE that addresses one row by its key, and returns
// sql.ErrNoRows when no row has that key — the answer a lookup of the row
// would give, taken from the write itself, so there is no gap between asking
// and writing.
//
// It rests on what SQLite counts as a changed row: every row the UPDATE's
// WHERE matched, whether or not the values written differ from the ones
// stored. So zero rows changed means the key matched nothing, and writing a
// row its current values is not mistaken for a missing row
// (TestUpdateRowTellsAMissingRowFromAnUnchangedOne).
func UpdateRow(d DBTX, query string, args ...any) error {
	res, err := d.Exec(query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func count(d DBTX, query string, args ...any) (int, error) {
	var n int
	err := d.QueryRow(query, args...).Scan(&n)
	return n, err
}

// queryInt64s collects a single int64 column from a query result set,
// replacing the repeated scan-into-int64 loops in functions like
// DistinctUserNodes and ChainsReferencingNode.
func queryInt64s(d DBTX, query string, args ...any) ([]int64, error) {
	rows, err := d.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
