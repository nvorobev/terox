package repl

import (
	"fmt"
	"strconv"
	"strings"
)

// sizesHeaders — колонки \sizes (ведущую "shard" добавляет renderDiagResults).
var sizesHeaders = []string{"table", "total_size", "indexes_size", "dead_pct"}

// sizesSQL строит запрос топ-N пользовательских таблиц по ПОЛНОМУ размеру отношения
// (куча + индексы + TOAST) на одном шарде, с долей мёртвых строк (раздутость). limit
// подставляется как целое (валидируется в doSizes).
func sizesSQL(limit int) string {
	return fmt.Sprintf(`SELECT n.nspname || '.' || c.relname AS "table",
       pg_size_pretty(pg_total_relation_size(c.oid)) AS total_size,
       pg_size_pretty(pg_indexes_size(c.oid)) AS indexes_size,
       CASE WHEN COALESCE(s.n_live_tup,0) + COALESCE(s.n_dead_tup,0) = 0 THEN '0%%'
            ELSE round(100.0 * COALESCE(s.n_dead_tup,0) / (COALESCE(s.n_live_tup,0) + COALESCE(s.n_dead_tup,0)), 1)::text || '%%' END AS dead_pct
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
WHERE c.relkind IN ('r','p') AND n.nspname NOT IN ('pg_catalog', 'information_schema')
ORDER BY pg_total_relation_size(c.oid) DESC
LIMIT %d`, limit)
}

// doSizes (\sizes [N]) печатает топ-N таблиц по полному размеру и долю мёртвых строк
// по каждому выбранному шарду (по умолчанию 20). На многих шардах ведущая колонка
// shard сразу показывает межшардовый скос размеров.
func (r *REPL) doSizes(args []string) error {
	limit := 20
	if len(args) > 0 {
		n, err := strconv.Atoi(strings.TrimSpace(args[0]))
		if err != nil || n <= 0 {
			return fmt.Errorf("\\sizes: N must be a positive integer, got %q", args[0])
		}
		if n > 200 {
			n = 200
		}
		limit = n
	}
	r.runDiagQuery(sizesSQL(limit), sizesHeaders)
	return nil
}
