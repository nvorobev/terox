// Package safety оценивает ИСПОЛНИТЕЛЬНЫЙ РИСК SQL (ExecutionRisk): read-only,
// волатильный побочный эффект, запись или безусловная запись — чтобы ограждать
// опасные запросы режимом записи и подтверждениями.
//
// ВАЖНО: это ЭВРИСТИКА, а НЕ доказанная граница безопасности. Классификатор по
// регуляркам не видит пользовательских функций/расширений и новых команд PostgreSQL.
// Настоящие границы безопасности — это (1) права роли подключения и (2)
// server-enforced read-only транзакция БД. Поэтому единый вход — Classify(sql),
// возвращающий Decision с уровнем риска и причинами, а не булево «безопасно/опасно».
// IsWrite/IsUnqualifiedWrite сохранены как удобные предикаты поверх той же модели.
package safety

import (
	"regexp"
	"strings"

	"terox/internal/sqlsplit"
)

// writeKeywords — начальные ключевые слова запросов, меняющих данные или схему.
var writeKeywords = map[string]bool{
	"insert":   true,
	"update":   true,
	"delete":   true,
	"truncate": true,
	"drop":     true,
	"alter":    true,
	"create":   true,
	"grant":    true,
	"revoke":   true,
	"comment":  true,
	"refresh":  true, // REFRESH MATERIALIZED VIEW пишет данные
	"reindex":  true,
	"cluster":  true,
	"vacuum":   true,
	"copy":     true, // COPY ... FROM пишет данные
	"call":     true, // процедуры могут писать
	"merge":    true,
	"reset":    true,
	"set":      true, // изменение сессии; трактуем консервативно
	"do":       true, // анонимный блок кода
	"lock":     true, // LOCK TABLE берёт блокировку (RO-транзакция её не блокирует)
}

var (
	leadingWord = regexp.MustCompile(`^[a-zA-Z]+`)
	// anyWriteRe ищет пишущее ключевое слово где угодно как целое слово;
	// помечает изменяющие данные CTE (WITH ... (DELETE ...) ...).
	anyWriteRe = regexp.MustCompile(`(?is)\b(insert|update|delete|truncate|merge|drop|alter|create)\b`)
	// sideEffectRe ищет волатильные функции с побочными эффектами, которые надо
	// считать записью. Большинство (advisory-блокировки, pg_reload_conf, сигналы
	// бэкендам, dblink, lo_*) разрешены внутри read-only транзакции, поэтому
	// эвристика — единственная защита; nextval/setval добавлены на всякий случай,
	// хотя read-only транзакция их и так блокирует.
	sideEffectRe = regexp.MustCompile(`(?is)\b(nextval|setval|pg_advisory_(xact_)?lock\w*|pg_try_advisory\w*|pg_advisory_unlock\w*|pg_logical_\w+|pg_terminate_backend|pg_cancel_backend|pg_reload_conf|pg_rotate_logfile|pg_create_restore_point|pg_promote|pg_switch_wal|pg_drop_replication_slot|pg_create_(physical|logical)_replication_slot|pg_replication_slot_advance|pg_replication_origin_\w+|pg_log_backend_memory_contexts|pg_read_(binary_)?file|pg_read_server_files|pg_ls_dir|pg_stat_file|pg_file_write|pg_file_unlink|dblink\w*|lo_(create|import|export|unlink|put)|lowrite|pg_stat_reset\w*|pg_stat_statements_reset|pg_import_system_collations|set_config)\s*\(`)
	// selectWriteRe ищет SELECT, которые всё же пишут или блокируют:
	// SELECT ... INTO newtable и конструкции блокировки строк.
	selectWriteRe = regexp.MustCompile(`(?is)(\binto\b|\bfor\s+update\b|\bfor\s+no\s+key\s+update\b|\bfor\s+share\b|\bfor\s+key\s+share\b)`)
)

// sanitize нейтрализует строковые литералы, идентификаторы в кавычках и
// комментарии (общим лексером), чтобы поиск ключевых слов не срабатывал на
// словах внутри них, затем приводит к нижнему регистру и обрезает пробелы.
// Единый проход с учётом литералов обязателен: `--` или `/* */` внутри строки
// в PostgreSQL — это литеральный текст, а не комментарий, а тело dollar-quoted
// или E-строки не должно утечь как код.
func sanitize(sql string) string {
	return strings.TrimSpace(strings.ToLower(sqlsplit.Mask(sql)))
}

// IsWrite сообщает, является ли sql (или может быть) пишущим запросом. Намеренно
// консервативна: всё, что не явно read-only, считается записью. Это лишь подсказка
// UX; настоящая граница защиты — read-only транзакция БД, кроме волатильных
// функций, которые read-only транзакция всё же разрешает (advisory-блокировки,
// pg_reload_conf, pg_cancel_backend/pg_terminate_backend, dblink, запись больших
// объектов, ...) — для них эвристика единственная защита. (nextval/setval
// блокируются read-only транзакцией; их пометка — подстраховка.)
//
// Ввод из нескольких запросов считается записью, если ХОТЯ БЫ один — запись.
func IsWrite(sql string) bool {
	stmts := sqlsplit.Split(sql)
	if len(stmts) > 1 {
		for _, s := range stmts {
			if isWriteSingle(s) {
				return true
			}
		}
		return false
	}
	return isWriteSingle(sql)
}

var whereRe = regexp.MustCompile(`(?is)\bwhere\b`)

// mainDMLRe ищет изменяющий данные глагол, который может идти после WITH.
var mainDMLRe = regexp.MustCompile(`(?is)\b(update|delete|truncate)\b`)

// mergeMatchedDMLRe ищет в MERGE действие WHEN MATCHED THEN UPDATE/DELETE.
// У MERGE нет верхнеуровневого WHERE — радиус задаёт ON-условие, поэтому такое
// действие может затронуть ВСЕ строки цели и требует усиленного подтверждения.
// Чистый WHEN NOT MATCHED THEN INSERT (вставка новых строк) сюда не попадает и
// корректно остаётся обычной записью.
var mergeMatchedDMLRe = regexp.MustCompile(`(?is)\bwhen\s+matched\b.*\bthen\s+(update|delete)\b`)

// topLevelParenGroups возвращает содержимое (без внешних скобок) каждой
// сбалансированной скобочной группы на глубине 1 в s. Используется для разбора
// тел CTE в WITH, каждое из которых обёрнуто в свои скобки.
func topLevelParenGroups(s string) []string {
	var groups []string
	depth := 0
	start := -1
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			if depth == 0 {
				start = i + 1
			}
			depth++
		case ')':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					groups = append(groups, s[start:i])
					start = -1
				}
			}
		}
	}
	return groups
}

// cteBodyIsUnqualified сообщает, является ли тело CTE (уже очищенное)
// изменяющим данные UPDATE/DELETE без WHERE верхнего уровня — т.е. оно меняет
// все строки цели. Изменяющий данные CTE в PostgreSQL выполняется всегда, даже
// если его результат не используется, поэтому требует усиленного подтверждения.
func cteBodyIsUnqualified(body string) bool {
	body = strings.TrimSpace(body)
	for strings.HasPrefix(body, "(") {
		body = strings.TrimSpace(body[1:])
	}
	switch leadingWord.FindString(body) {
	case "update", "delete":
		return !whereRe.MatchString(removeParens(body))
	}
	return false
}

// removeParens убирает скобочные группы, чтобы WHERE внутри подзапроса не
// считался собственным WHERE запроса.
func removeParens(s string) string {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteByte(s[i])
			}
		}
	}
	return b.String()
}

// analyzeOptionRe ищет ANALYZE/ANALYSE как ОПЦИЮ внутри скобочного списка опций
// EXPLAIN, например `(analyze)`, `(analyze, verbose)`, `(analyze true)`, `(analyze on)`.
// Совпадает только когда analyze — отдельное слово, начинающее опцию (после '(' или
// ','), чтобы имя объекта вроде analyze_log в скобках не считалось опцией.
var analyzeOptionRe = regexp.MustCompile(`(?is)[(,]\s*analy[sz]e\b`)

// stripExplainAnalyze разворачивает `EXPLAIN [(опции)] [ANALYZE|VERBOSE ...] <stmt>`,
// возвращая вложенный <stmt> и true ТОЛЬКО для EXPLAIN ANALYZE/ANALYSE — варианта,
// который реально выполняет вложенный запрос. Для обычного EXPLAIN (без ANALYZE) и
// для не-EXPLAIN возвращает ("", false). Вход — уже очищенный sanitize-ом текст.
// Единый источник правды для обоих классификаторов (IsWrite и IsUnqualifiedWrite).
//
// ANALYZE детектируется как ОПЦИЯ EXPLAIN (ключевое слово сразу после EXPLAIN или
// внутри скобочного списка опций), а НЕ как любая подстрока: иначе имя объекта,
// содержащее "analyze" (например `EXPLAIN DELETE FROM analyze_log`), ложно считалось
// бы исполняющим запись. Реальные `EXPLAIN ANALYZE <write>`, `EXPLAIN (ANALYZE) <write>`,
// `EXPLAIN (ANALYZE TRUE) <write>`, `EXPLAIN (ANALYZE on) <write>` по-прежнему
// распознаются как исполняющие вложенный запрос (НЕ read-only).
func stripExplainAnalyze(clean string) (string, bool) {
	if leadingWord.FindString(clean) != "explain" {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(clean, "explain"))
	analyzed := false
	// Скобочный список опций, например EXPLAIN (ANALYZE, VERBOSE) ... Здесь ANALYZE
	// надо искать как опцию (начало элемента списка), а не как подстроку: `(analyze)`,
	// `(analyze, verbose)`, `(analyze true)`, `(analyze on)` — это исполнение, а имя
	// объекта внутри не относится к опциям. Открывающую '(' добавляем к содержимому,
	// чтобы первая опция тоже распозналась через разделитель.
	if strings.HasPrefix(rest, "(") {
		if i := strings.IndexByte(rest, ')'); i >= 0 {
			opts := rest[:i+1] // включая внешние скобки
			if analyzeOptionRe.MatchString(opts) {
				analyzed = true
			}
			rest = strings.TrimSpace(rest[i+1:])
		}
	}
	// Голые ведущие ключевые слова опций (EXPLAIN ANALYZE VERBOSE ...). ANALYZE/ANALYSE
	// сразу после EXPLAIN — это опция исполнения, а не имя объекта.
	for {
		w := leadingWord.FindString(rest)
		if w == "analyze" || w == "analyse" || w == "verbose" {
			if w == "analyze" || w == "analyse" {
				analyzed = true
			}
			rest = strings.TrimSpace(strings.TrimPrefix(rest, w))
			continue
		}
		break
	}
	if !analyzed {
		return "", false
	}
	return rest, true
}

// IsUnqualifiedWrite сообщает, является ли sql UPDATE/DELETE/TRUNCATE,
// затрагивающим ВСЕ строки: UPDATE или DELETE без WHERE верхнего уровня, либо
// TRUNCATE. Используется для запроса усиленного подтверждения.
func IsUnqualifiedWrite(sql string) bool {
	return isUnqualifiedClean(sanitize(sql))
}

// isUnqualifiedClean классифицирует уже очищенный (sanitize) запрос как
// безусловную запись. Вынесено отдельно, чтобы рекурсивно переклассифицировать
// тело EXPLAIN ANALYZE.
func isUnqualifiedClean(clean string) bool {
	for strings.HasPrefix(clean, "(") {
		clean = strings.TrimSpace(clean[1:])
	}
	// EXPLAIN ANALYZE реально выполняет вложенный запрос, поэтому безусловный DML
	// под ним (TRUNCATE/DELETE/UPDATE без WHERE) требует того же усиленного
	// подтверждения. Обычный EXPLAIN ничего не исполняет и сюда не относится.
	if rest, ok := stripExplainAnalyze(clean); ok {
		return isUnqualifiedClean(rest)
	}
	switch leadingWord.FindString(clean) {
	case "truncate":
		return true
	case "merge":
		// MERGE с действием WHEN MATCHED THEN UPDATE/DELETE — потенциально
		// безусловная массовая запись: радиус задаёт ON-условие соединения, а
		// верхнеуровневого WHERE нет, поэтому WHERE-эвристика неприменима.
		// Консервативно требуем усиленного подтверждения. Чистый WHEN NOT MATCHED
		// THEN INSERT остаётся обычной записью.
		return mergeMatchedDMLRe.MatchString(clean)
	case "update", "delete":
		return !whereRe.MatchString(removeParens(clean))
	case "with":
		// WITH может предшествовать изменяющему данные запросу, например
		// `WITH c AS (...) UPDATE t SET x=1` (без WHERE) меняет все строки. Убираем
		// скобочные тела CTE/подзапросы и смотрим завершающий глагол верхнего
		// уровня. (Пишущее тело CTE без WHERE всё равно помечается как запись в
		// IsWrite и подтверждается; здесь — только *усиленный* барьер.)
		//
		// Само изменяющее данные тело CTE может быть безусловной записью, даже
		// если завершающий запрос — безобидный SELECT, например
		// `WITH d AS (DELETE FROM users RETURNING *) SELECT count(*) FROM d`.
		// Такой CTE выполняется всегда, поэтому сначала сканируем каждое тело CTE
		// на безусловный UPDATE/DELETE, а затем переходим к завершающему глаголу.
		for _, grp := range topLevelParenGroups(clean) {
			if cteBodyIsUnqualified(grp) {
				return true
			}
		}
		body := removeParens(clean)
		loc := mainDMLRe.FindStringIndex(body)
		if loc == nil {
			return false
		}
		if body[loc[0]:loc[1]] == "truncate" {
			return true
		}
		return !whereRe.MatchString(body[loc[1]:])
	}
	return false
}

// AnyUnqualifiedWrite сообщает, есть ли в скрипте хотя бы один безусловный
// пишущий запрос.
func AnyUnqualifiedWrite(script string) bool {
	for _, s := range sqlsplit.Split(script) {
		if IsUnqualifiedWrite(s) {
			return true
		}
	}
	return false
}

// isWriteSingle классифицирует один запрос.
func isWriteSingle(sql string) bool {
	clean := sanitize(sql)
	if clean == "" {
		return false
	}
	// Убираем ведущие скобки, например "(with d as (delete ...) ...)".
	for strings.HasPrefix(clean, "(") {
		clean = strings.TrimSpace(clean[1:])
	}

	// Волатильные функции с побочными эффектами пишут даже из ведущего SELECT и
	// не блокируются read-only транзакцией. Проверяем и стандартный замаскированный
	// текст, И вариант с видимым содержимым идентификаторов в кавычках, поскольку
	// имя функции в двойных кавычках (SELECT "dblink_exec"(...),
	// public."pg_terminate_backend"(pid)) иначе превратится в 'x' при sanitize/Mask
	// и проскользнёт мимо чёрного списка.
	if sideEffectRe.MatchString(clean) ||
		sideEffectRe.MatchString(strings.ToLower(sqlsplit.MaskKeepQuoted(sql))) {
		return true
	}

	first := leadingWord.FindString(clean)

	// CTE в WITH (...) могут скрывать пишущий запрос где угодно внутри, а финальный
	// SELECT сам может писать (SELECT ... INTO) или блокировать (FOR SHARE/UPDATE).
	if first == "with" {
		return anyWriteRe.MatchString(clean) || selectWriteRe.MatchString(clean)
	}

	// EXPLAIN ANALYZE действительно выполняет вложенный запрос; обычный EXPLAIN —
	// нет. Разворачиваем EXPLAIN ANALYZE общим хелпером и переклассифицируем
	// вложенный запрос; обычный EXPLAIN исполнением не является → read-only.
	if first == "explain" {
		rest, ok := stripExplainAnalyze(clean)
		if !ok {
			return false
		}
		return isWriteSingle(rest)
	}

	switch first {
	case "select":
		// SELECT ... INTO newtable пишет; SELECT ... FOR UPDATE/SHARE блокирует.
		return selectWriteRe.MatchString(clean)
	case "show", "table", "values", "fetch":
		return false
	}
	if writeKeywords[first] {
		return true
	}
	// Модель белого списка: чтением считаются только явно read-only формы выше
	// (select / show / table / values / fetch / with / explain без analyze). ЛЮБОЕ
	// другое ведущее ключевое слово — включая не перечисленные (LOAD, CHECKPOINT,
	// PREPARE/EXECUTE, DISCARD, LISTEN/NOTIFY, SECURITY LABEL, IMPORT FOREIGN
	// SCHEMA, голый BEGIN, ...) — не доказуемо read-only, поэтому считается
	// потенциальной ЗАПИСЬЮ и ограждается. Это консервативное направление:
	// ошибочно помеченное чтение лишь попросит подтверждение, тогда как ошибочно
	// пропущенная запись может незащищённо изменить прод.
	return true
}
