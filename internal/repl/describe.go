package repl

import (
	"fmt"

	"terox/internal/db"
	"terox/internal/render"
	"terox/internal/ui"
)

// doDescribe (\d <table>) показывает структуру таблицы на ПЕРВОМ выбранном шарде:
// колонки, индексы, внешние ключи, ссылки на неё (referenced by), check-ограничения и
// полный размер. Для межшардового дрейфа схемы есть отдельная команда \diff.
func (r *REPL) doDescribe(tableArg string) error {
	if len(r.targets) == 0 {
		return fmt.Errorf("no shard selected")
	}
	tbl, err := parseTableArg(tableArg)
	if err != nil {
		return fmt.Errorf("\\d: %v", err)
	}
	shard := r.targets[0]
	lit := sqlLiteral(tbl)

	cols, isErr := r.docQuery(shard, fmt.Sprintf(describeTableSQL, lit))
	if isErr || cols == nil {
		return fmt.Errorf("\\d %s: could not read the table on %s", tbl, shard.Label)
	}
	if len(cols.Rows) == 0 {
		return fmt.Errorf("\\d: no table %q visible on %s", tbl, shard.Label)
	}

	header := tbl
	if len(r.targets) > 1 {
		header += fmt.Sprintf("  — first-shard sample on %s; \\diff %s for cross-shard drift", shard.Label, tbl)
	}
	fmt.Fprintln(r.out, ui.Service.Render(header))
	r.describeSection("Columns", cols)

	for _, sec := range []struct {
		title, sql string
	}{
		{"Indexes", describeIndexesSQL},
		{"Foreign keys", describeForeignKeysSQL},
		{"Referenced by", describeReferencedBySQL},
		{"Check constraints", describeChecksSQL},
		{"Size", describeSizeSQL},
	} {
		if res, _ := r.docQuery(shard, fmt.Sprintf(sec.sql, lit)); res != nil && len(res.Rows) > 0 {
			r.describeSection(sec.title, res)
		}
	}
	return nil
}

// describeSection печатает озаглавленную секцию \d одной таблицей.
func (r *REPL) describeSection(title string, res *db.Result) {
	fmt.Fprintf(r.out, "\n%s\n", ui.Dim.Render(title))
	render.Single(r.out, res, r.maxRows, false)
}

// describeIndexesSQL — индексы таблицы (с пометкой primary/unique и INVALID).
const describeIndexesSQL = `SELECT c2.relname AS "index",
    CASE WHEN i.indisprimary THEN 'primary' WHEN i.indisunique THEN 'unique' ELSE '' END AS "kind",
    CASE WHEN i.indisvalid AND i.indisready THEN '' ELSE 'INVALID' END AS "state",
    pg_get_indexdef(i.indexrelid) AS "definition"
FROM pg_index i
JOIN pg_class c2 ON c2.oid = i.indexrelid
WHERE i.indrelid = to_regclass(%s)
ORDER BY i.indisprimary DESC, c2.relname`

// describeForeignKeysSQL — внешние ключи самой таблицы.
const describeForeignKeysSQL = `SELECT conname AS "name", pg_get_constraintdef(oid) AS "definition"
FROM pg_constraint
WHERE conrelid = to_regclass(%s) AND contype = 'f'
ORDER BY conname`

// describeReferencedBySQL — внешние ключи ДРУГИХ таблиц, ссылающиеся на эту (основа
// навигации по связям).
const describeReferencedBySQL = `SELECT conrelid::regclass::text AS "table", conname AS "name", pg_get_constraintdef(oid) AS "definition"
FROM pg_constraint
WHERE confrelid = to_regclass(%s) AND contype = 'f'
ORDER BY conrelid::regclass::text, conname`

// describeChecksSQL — check-ограничения таблицы.
const describeChecksSQL = `SELECT conname AS "name", pg_get_constraintdef(oid) AS "definition"
FROM pg_constraint
WHERE conrelid = to_regclass(%s) AND contype = 'c'
ORDER BY conname`

// describeSizeSQL — полный размер таблицы (куча/индексы/всего).
const describeSizeSQL = `WITH t AS (SELECT to_regclass(%s) AS oid)
SELECT pg_size_pretty(pg_table_size(oid)) AS "table",
       pg_size_pretty(pg_indexes_size(oid)) AS "indexes",
       pg_size_pretty(pg_total_relation_size(oid)) AS "total"
FROM t WHERE oid IS NOT NULL`
