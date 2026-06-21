// Package advisor содержит ЧИСТУЮ эвристику index-advisor поверх текста плана
// EXPLAIN (без ANALYZE — запрос не выполняется). Анализирует ТОЛЬКО текст плана
// (без живой статистики), поэтому это ЭВРИСТИКА, а не полноценный advisor: он не
// знает селективности, размеров таблиц, стоимости записи и partial/expression
// индексов. Каждое предложение помечено уровнем уверенности и доказательством из
// плана; авто-создание индекса по одной эвристике запрещено — выдаётся только DDL
// с rollback.
//
// Покрываемые источники предложений:
//   - filter   — Seq Scan с Filter (равенства → ведущие столбцы, диапазон → хвост);
//   - join     — Hash/Merge/Index Cond и Join Filter (столбцы join по Seq-Scan стороне);
//   - sort     — Sort Key над одиночной Seq-Scan таблицей (индекс под ORDER BY);
//   - group    — Group Key (Sorted/Hashed Aggregate) над одиночной Seq-Scan таблицей.
//
// Пакет чистый (зависит только от terox/internal/explain) и тестируется без БД.
package advisor

import (
	"fmt"
	"regexp"
	"strings"

	"terox/internal/explain"
)

// sqlLiteral экранирует строку как SQL-литерал (локальная копия repl-хелпера, чтобы
// пакет не зависел от repl).
func sqlLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// cellStr приводит ячейку результата к строке (локальная копия repl.str).
func cellStr(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// Proposal — одно предложение индекса с доказательством и уверенностью.
type Proposal struct {
	Schema, Table string
	Cols          []string // порядок важен: ведущие столбцы первыми
	Kind          string   // filter|join|sort|group
	Confidence    string   // high|medium|low
	Evidence      string
	LikeLead      bool // ведущий столбец используется с LIKE → подсказка про text_pattern_ops
}

// IndexName возвращает предлагаемое имя индекса (idx_<table>_<col…>).
func (p Proposal) IndexName() string {
	return "idx_" + p.Table + "_" + strings.Join(p.Cols, "_")
}

func (p Proposal) key() string {
	return p.Kind + "|" + p.Schema + "." + p.Table + "(" + strings.ToLower(strings.Join(p.Cols, ",")) + ")"
}

// scanRel — базовое отношение, прочитанное узлом скана (для разрешения
// qualifier→таблица в join/sort/group и проверки, что таблица именно Seq-Scan'ится).
type scanRel struct {
	schema, table string
	seq           bool
}

// collectScanAliases строит карту alias/имя_отношения → базовое отношение по всем
// узлам скана. EXPLAIN VERBOSE квалифицирует столбцы псевдонимом (или именем
// таблицы, если псевдонима нет), поэтому карта позволяет привязать join/sort/group
// столбец к конкретной таблице. Если имя неоднозначно (одно и то же у двух
// отношений), помечаем его как неразрешимое (nil), чтобы не выдать неверную таблицу.
func collectScanAliases(n *explain.Node, out map[string]*scanRel) {
	if n.RelationName != "" && strings.Contains(n.NodeType, "Scan") {
		rel := &scanRel{schema: n.Schema, table: n.RelationName, seq: n.NodeType == "Seq Scan"}
		for _, name := range []string{n.Alias, n.RelationName} {
			if name == "" {
				continue
			}
			k := strings.ToLower(name)
			if prev, ok := out[k]; ok {
				if prev == nil || prev.table != rel.table || prev.schema != rel.schema {
					out[k] = nil // неоднозначно → не разрешаем
				}
				continue
			}
			out[k] = rel
		}
	}
	for i := range n.Plans {
		collectScanAliases(&n.Plans[i], out)
	}
}

// filterColRe ловит "столбец <оператор>" в тексте Filter из EXPLAIN.
var filterColRe = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(=|<>|>=|<=|>|<|~~\*?|!~~?\*?)`)

// castRe убирает суффикс приведения типа `::тип` (включая многословные типы вроде
// `character varying`, размер/массив), чтобы имя типа не путалось со столбцом.
var castRe = regexp.MustCompile(`(?i)::\s*"?[a-z_][a-z0-9_ ]*"?(\(\s*\d+\s*(,\s*\d+\s*)?\))?(\[\s*\])?`)

// parenIdentRe разворачивает скобки вокруг (возможно квалифицированного)
// идентификатора: EXPLAIN VERBOSE рендерит неявное приведение слева как
// `(rel.col)::type`, и после снятия `::type` остаётся `(rel.col)`, который надо
// развернуть до `rel.col` (точки разрешены, чтобы поймать квалифицированный столбец).
var parenIdentRe = regexp.MustCompile(`\(\s*([a-zA-Z_][a-zA-Z0-9_.]*)\s*\)`)

var filterStopWords = map[string]bool{
	"and": true, "or": true, "not": true, "true": true, "false": true,
	"null": true, "is": true, "in": true, "any": true, "case": true, "when": true,
}

// simpleIdentRe — простой нижнерегистровый идентификатор, не требующий кавычек.
var simpleIdentRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// quoteIdentDDL цитирует идентификатор для копипаст-DDL, если он не простой
// нижнерегистровый (mixed-case/спецсимволы), удваивая внутренние кавычки.
func quoteIdentDDL(s string) string {
	if simpleIdentRe.MatchString(s) {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// normalizeFilter готовит текст Filter к извлечению столбцов: снимает приведения
// типов и разворачивает `(col)` → `col`, чтобы `((name)::text = 'x'::text)`
// давал столбец `name`, а не имя типа `text`.
func normalizeFilter(filter string) string {
	s := castRe.ReplaceAllString(filter, "")
	for i := 0; i < 3; i++ { // несколько проходов на вложенные скобки
		ns := parenIdentRe.ReplaceAllString(s, "$1")
		if ns == s {
			break
		}
		s = ns
	}
	return s
}

// filterColumns извлекает столбцы-кандидаты из текста Filter. Столбцы из равенств
// (=) идут первыми (лучшие кандидаты для индекса), затем диапазонные/LIKE.
func filterColumns(filter string) []string {
	var eq, other []string
	seen := map[string]bool{}
	for _, m := range filterColRe.FindAllStringSubmatch(normalizeFilter(filter), -1) {
		col, op := m[1], m[2]
		lc := strings.ToLower(col)
		if filterStopWords[lc] || seen[lc] {
			continue
		}
		seen[lc] = true
		if op == "=" {
			eq = append(eq, col)
		} else {
			other = append(other, col)
		}
	}
	return append(eq, other...)
}

// qualColRe ловит квалифицированный столбец rel.col в тексте условия/ключа.
var qualColRe = regexp.MustCompile(`([a-zA-Z_][\w$]*)\.([a-zA-Z_][\w$]*)`)

// sortDirRe убирает хвост элемента Sort Key: направление и NULLS-порядок, а также
// `USING op`, чтобы остался только сам столбец/выражение.
var sortDirRe = regexp.MustCompile(`(?i)\s+(asc|desc|nulls\s+(first|last)|using\s+\S+)\b`)

type qualCol struct{ qual, col string }

// condColumns извлекает квалифицированные столбцы rel.col из текста условия
// (Hash/Merge/Index Cond, Join Filter). Приведения типов снимаются, чтобы имя типа
// не путалось со столбцом. Порядок сохраняется, дубли (rel.col) убираются.
func condColumns(cond string) []qualCol {
	cond = castRe.ReplaceAllString(cond, "")
	var out []qualCol
	seen := map[string]bool{}
	for _, m := range qualColRe.FindAllStringSubmatch(cond, -1) {
		qc := qualCol{strings.ToLower(m[1]), m[2]}
		k := qc.qual + "." + strings.ToLower(qc.col)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, qc)
	}
	return out
}

// resolveSeqRel возвращает базовое отношение для qualifier, если оно однозначно и
// читается Seq Scan'ом (индекс на нём реально мог бы помочь). Иначе ok=false.
func resolveSeqRel(aliases map[string]*scanRel, qual string) (*scanRel, bool) {
	rel, ok := aliases[strings.ToLower(qual)]
	if !ok || rel == nil || !rel.seq {
		return nil, false
	}
	return rel, true
}

// filterIndexCols выбирает ведущие столбцы составного индекса из Filter: все
// столбцы-равенства (хорошие ведущие) и ОДИН диапазонный/LIKE в хвосте — таков
// стандартный приём (равенства первыми, затем один range). filterColumns уже
// возвращает eq-столбцы раньше прочих.
func filterIndexCols(filter string) []string {
	cands := filterColumns(filter)
	if len(cands) == 0 {
		return nil
	}
	// Разделяем на eq и прочие, повторно используя нормализацию filterColumns:
	// первый «не-eq» допускаем как хвост, дальнейшие диапазоны индексу не помогают.
	eq, rest := splitEqRange(filter)
	if len(eq) == 0 {
		// Нет равенств — один (ведущий) диапазонный столбец.
		return cands[:1]
	}
	cols := append([]string{}, eq...)
	if len(rest) > 0 {
		cols = append(cols, rest[0])
	}
	return cols
}

// splitEqRange повторяет разбор filterColumns, но разделяет столбцы равенств и
// диапазонов/LIKE (для построения составного индекса eq…+range).
func splitEqRange(filter string) (eq, other []string) {
	seen := map[string]bool{}
	for _, m := range filterColRe.FindAllStringSubmatch(normalizeFilter(filter), -1) {
		col, op := m[1], m[2]
		lc := strings.ToLower(col)
		if filterStopWords[lc] || seen[lc] {
			continue
		}
		seen[lc] = true
		if op == "=" {
			eq = append(eq, col)
		} else {
			other = append(other, col)
		}
	}
	return eq, other
}

// CollectProposals собирает предложения из всего плана: фильтры, join, sort, group.
// Чистая функция (работает по дереву плана + карте алиасов), поэтому тестируется
// без БД. Дубли по (kind, таблица, столбцы) схлопываются.
func CollectProposals(root *explain.Node) []Proposal {
	aliases := map[string]*scanRel{}
	collectScanAliases(root, aliases)
	var props []Proposal
	seen := map[string]bool{}
	add := func(p Proposal) {
		if len(p.Cols) == 0 {
			return
		}
		if seen[p.key()] {
			return
		}
		seen[p.key()] = true
		props = append(props, p)
	}
	var walk func(n *explain.Node)
	walk = func(n *explain.Node) {
		// filter: Seq Scan с Filter.
		if n.NodeType == "Seq Scan" && n.RelationName != "" && strings.TrimSpace(n.Filter) != "" {
			if cols := filterIndexCols(n.Filter); len(cols) > 0 {
				conf := "high"
				if len(cols) > 1 {
					conf = "medium" // составной — селективность хвоста не доказана
				}
				leadLike := patternColumns(n.Filter)[strings.ToLower(cols[0])]
				add(Proposal{Schema: n.Schema, Table: n.RelationName, Cols: cols, Kind: "filter", Confidence: conf,
					Evidence: fmt.Sprintf("seq scan on %s filters on %s", n.RelationName, strings.Join(cols, ", ")),
					LikeLead: leadLike})
			}
		}
		// join: условия соединения по Seq-Scan стороне.
		for _, cond := range []string{n.HashCond, n.MergeCond, n.IndexCond, n.JoinFilter} {
			if strings.TrimSpace(cond) == "" {
				continue
			}
			for _, qc := range condColumns(cond) {
				if rel, ok := resolveSeqRel(aliases, qc.qual); ok {
					add(Proposal{Schema: rel.schema, Table: rel.table, Cols: []string{qc.col}, Kind: "join", Confidence: "medium",
						Evidence: fmt.Sprintf("%s join probes %s.%s with a sequential scan", n.NodeType, qc.qual, qc.col)})
				}
			}
		}
		// sort / group: ключи над ОДИНОЧНОЙ Seq-Scan таблицей.
		if len(n.SortKey) > 0 {
			if rel, cols := singleRelKeyCols(aliases, n.SortKey); rel != nil {
				add(Proposal{Schema: rel.schema, Table: rel.table, Cols: cols, Kind: "sort", Confidence: "low",
					Evidence: fmt.Sprintf("sort on %s could use an index matching ORDER BY %s", rel.table, strings.Join(cols, ", "))})
			}
		}
		if len(n.GroupKey) > 0 {
			if rel, cols := singleRelKeyCols(aliases, n.GroupKey); rel != nil {
				add(Proposal{Schema: rel.schema, Table: rel.table, Cols: cols, Kind: "group", Confidence: "low",
					Evidence: fmt.Sprintf("grouping on %s could use an index on %s", rel.table, strings.Join(cols, ", "))})
			}
		}
		for i := range n.Plans {
			walk(&n.Plans[i])
		}
	}
	walk(root)
	return props
}

// isColPrefix сообщает, является ли a префиксом b (по именам столбцов, без учёта
// регистра). Индекс на (a,b,c) покрывает запросы по префиксу (a) и (a,b).
func isColPrefix(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// DedupeOverlap убирает предложения, чьи столбцы — префикс другого предложения по
// ТОЙ ЖЕ таблице (составной индекс (a,b) поглощает одностолбцовый (a), а join (a)
// дублирует sort (a)). Оставляет самое длинное покрывающее предложение. Сохраняет
// порядок. Чистая функция — тестируется без БД. Дополняет точечный dedup по ключу.
func DedupeOverlap(props []Proposal) []Proposal {
	keep := make([]bool, len(props))
	for i := range props {
		keep[i] = true
	}
	for i := range props {
		if !keep[i] {
			continue
		}
		for j := range props {
			if i == j || !keep[j] {
				continue
			}
			// props[i] поглощается props[j], если на той же таблице cols[i] —
			// префикс более длинного cols[j].
			if props[i].Schema == props[j].Schema && props[i].Table == props[j].Table &&
				len(props[i].Cols) < len(props[j].Cols) && isColPrefix(props[i].Cols, props[j].Cols) {
				keep[i] = false
				break
			}
		}
	}
	out := props[:0:0]
	for i, p := range props {
		if keep[i] {
			out = append(out, p)
		}
	}
	return out
}

// CoveredByExisting сообщает, покрыты ли cols каким-либо существующим индексом —
// т.е. cols является префиксом столбцов индекса (порядок важен). existing — список
// упорядоченных списков столбцов индексов таблицы.
func CoveredByExisting(cols []string, existing [][]string) bool {
	for _, idx := range existing {
		if isColPrefix(cols, idx) {
			return true
		}
	}
	return false
}

// patternColRe ловит столбец, используемый с LIKE (~~) — для подсказки об opclass
// text_pattern_ops (помогает левостороннему LIKE 'prefix%' вне C-локали).
var patternColRe = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(~~\*?)`)

// patternColumns возвращает множество столбцов, к которым применяется LIKE.
func patternColumns(filter string) map[string]bool {
	out := map[string]bool{}
	for _, m := range patternColRe.FindAllStringSubmatch(normalizeFilter(filter), -1) {
		out[strings.ToLower(m[1])] = true
	}
	return out
}

// singleRelKeyCols разбирает список ключей Sort/Group: снимает ASC/DESC/NULLS,
// извлекает rel.col и требует, чтобы ВСЕ ключи указывали на одну и ту же
// Seq-Scan таблицу (тогда составной индекс под ними осмыслен). Выражения
// (lower(...), (a+b)) и неразрешимые/смешанные таблицы → отказ (nil).
func singleRelKeyCols(aliases map[string]*scanRel, keys []string) (*scanRel, []string) {
	var rel *scanRel
	var cols []string
	for _, k := range keys {
		k = sortDirRe.ReplaceAllString(k, "")
		k = strings.TrimSpace(k)
		m := qualColRe.FindStringSubmatch(k)
		if m == nil {
			return nil, nil // выражение или неквалифицированный ключ — не индексируем просто
		}
		// Ключ должен быть ровно rel.col, а не выражение, содержащее его.
		if m[0] != k {
			return nil, nil
		}
		r, ok := resolveSeqRel(aliases, m[1])
		if !ok {
			return nil, nil
		}
		if rel == nil {
			rel = r
		} else if rel.table != r.table || rel.schema != r.schema {
			return nil, nil // ключи из разных таблиц — общий индекс не подходит
		}
		cols = append(cols, m[2])
	}
	return rel, cols
}

// IndexColumnsSQL — УПОРЯДОЧЕННЫЕ столбцы каждого btree-индекса таблицы (по одной
// строке на (индекс, столбец, позиция)), чтобы проверять покрытие предложения ПОЛНЫМ
// префиксом существующего индекса, а не только ведущим столбцом. Частичные/невалидные
// индексы и выражения (attnum=0) исключаются. schema может быть пуст.
func IndexColumnsSQL(schema, table string) string {
	where := "t.relname = " + sqlLiteral(table)
	if schema != "" {
		where += " AND n.nspname = " + sqlLiteral(schema)
	}
	return `SELECT i.indexrelid::text AS idx, a.attname, k.ord
FROM pg_index i
JOIN pg_class t ON t.oid = i.indrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
WHERE ` + where + `
  AND i.indpred IS NULL AND i.indisvalid AND i.indisready AND k.attnum <> 0
ORDER BY i.indexrelid, k.ord`
}

// TableStatsSQL — оценка размера таблицы (строки и байты кучи) для оценки пользы/цены
// индекса: на крошечной таблице индекс редко помогает; на большой — стоит storage и
// write-amplification. schema может быть пуст.
func TableStatsSQL(schema, table string) string {
	where := "c.relname = " + sqlLiteral(table)
	if schema != "" {
		where += " AND n.nspname = " + sqlLiteral(schema)
	}
	return `SELECT c.reltuples::bigint AS est_rows,
       pg_relation_size(c.oid) AS heap_bytes,
       pg_size_pretty(pg_relation_size(c.oid)) AS heap_size
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE ` + where + ` AND c.relkind IN ('r','p')`
}

// ColumnStatsSQL — селективность ведущего столбца из pg_stats: n_distinct (>0 —
// абсолютное число различных значений; <0 — доля от числа строк) и null_frac.
// Низкая кардинальность (мало различных значений) делает обычный btree малополезным.
// schema может быть пуст.
func ColumnStatsSQL(schema, table, col string) string {
	where := "tablename = " + sqlLiteral(table) + " AND attname = " + sqlLiteral(col)
	if schema != "" {
		where += " AND schemaname = " + sqlLiteral(schema)
	}
	return "SELECT n_distinct, null_frac FROM pg_stats WHERE " + where + " LIMIT 1"
}

// SelectivityNote форматирует подсказку о кардинальности ведущего столбца по
// n_distinct из pg_stats. "" — если статистики нет (таблица не проанализирована).
func SelectivityNote(nDistinct float64) string {
	switch {
	case nDistinct == 0:
		return "" // нет статистики
	case nDistinct < 0:
		// доля: -1 = все значения уникальны (отличная селективность).
		pct := -nDistinct * 100
		return fmt.Sprintf("leading column ~%.0f%% distinct (per pg_stats) — higher is more selective", pct)
	case nDistinct <= 10:
		return fmt.Sprintf("leading column has only ~%.0f distinct value(s) (per pg_stats) — LOW cardinality, a plain btree may not help; consider a partial index", nDistinct)
	default:
		return fmt.Sprintf("leading column ~%.0f distinct value(s) (per pg_stats)", nDistinct)
	}
}

// ParseIndexColumns группирует строки IndexColumnsSQL (idx, attname, ord) в
// упорядоченные списки столбцов по каждому индексу.
func ParseIndexColumns(rows [][]any) [][]string {
	order := []string{}
	byIdx := map[string][]string{}
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		idx := cellStr(row[0])
		if _, ok := byIdx[idx]; !ok {
			order = append(order, idx)
		}
		byIdx[idx] = append(byIdx[idx], strings.ToLower(cellStr(row[1])))
	}
	out := make([][]string, 0, len(order))
	for _, idx := range order {
		out = append(out, byIdx[idx])
	}
	return out
}

// IndexDDL строит CREATE/DROP DDL для предложения: имя idx_<table>_<col…>,
// идентификаторы цитируются для копипаста. rollback использует IF EXISTS, чтобы не
// падать, если индекс ещё не создавали.
func (p Proposal) IndexDDL() (suggest, rollback string) {
	rel := quoteIdentDDL(p.Table)
	if p.Schema != "" {
		rel = quoteIdentDDL(p.Schema) + "." + quoteIdentDDL(p.Table)
	}
	idx := quoteIdentDDL("idx_" + p.Table + "_" + strings.Join(p.Cols, "_"))
	quoted := make([]string, len(p.Cols))
	for i, c := range p.Cols {
		quoted[i] = quoteIdentDDL(c)
	}
	suggest = fmt.Sprintf("CREATE INDEX CONCURRENTLY %s ON %s (%s);", idx, rel, strings.Join(quoted, ", "))
	rollback = fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %s;", idx)
	return suggest, rollback
}

// SharesLeadingColumn сообщает, есть ли существующий индекс с тем же ведущим
// столбцом, что и cols[0] (но не покрывающий cols целиком) — тогда новый составной
// индекс всё ещё может помочь.
func SharesLeadingColumn(cols []string, existing [][]string) bool {
	if len(cols) == 0 {
		return false
	}
	for _, idx := range existing {
		if len(idx) > 0 && strings.EqualFold(idx[0], cols[0]) {
			return true
		}
	}
	return false
}

// QuoteIdentDDL экспортирует quoteIdentDDL для построения DDL-подсказок в repl
// (opclass-хинт). Идентификатор цитируется, если он не простой нижнерегистровый.
func QuoteIdentDDL(s string) string { return quoteIdentDDL(s) }
