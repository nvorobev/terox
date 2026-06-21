package repl

import (
	"fmt"
	"sort"
	"strings"

	"terox/internal/cluster"
	"terox/internal/ui"
)

// settingsOfInterest — параметры конфигурации, заметно влияющие на планы
// запросов, память и поведение.
var settingsOfInterest = []string{
	"work_mem", "maintenance_work_mem", "shared_buffers", "effective_cache_size",
	"random_page_cost", "seq_page_cost", "cpu_tuple_cost", "effective_io_concurrency",
	"max_parallel_workers_per_gather", "max_parallel_workers", "max_worker_processes",
	"jit", "jit_above_cost", "default_statistics_target", "from_collapse_limit",
	"join_collapse_limit", "max_wal_size", "checkpoint_completion_target",
	"autovacuum", "autovacuum_vacuum_scale_factor", "autovacuum_analyze_scale_factor",
	"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout",
}

// doCompare сравнивает текущее хранилище с другим (схема, индексы, конфиг),
// отвечая на вопрос «почему здесь поведение отличается».
//
//	\compare <service/storage>
func (r *REPL) doCompare(args []string) error {
	if len(r.targets) == 0 {
		return fmt.Errorf("no current storage selected")
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: \\compare <service/storage>")
	}
	parts := strings.SplitN(args[0], "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("target must be service/storage, got %q", args[0])
	}
	svc, ok := r.cfg.Services[parts[0]]
	if !ok || svc == nil {
		return fmt.Errorf("unknown service %q", parts[0])
	}
	st, ok := svc.Storages[parts[1]]
	if !ok || st == nil {
		return fmt.Errorf("unknown or empty storage %q in service %q", parts[1], parts[0])
	}
	otherShards, err := cluster.Expand(st)
	if err != nil {
		return err
	}

	a := r.targets[0]
	b := otherShards[0]
	aName := r.service + "/" + r.storage + "(" + a.Label + ")"
	bName := args[0] + "(" + b.Label + ")"
	// \compare сравнивает ПО ОДНОМУ шарду с каждой стороны. Если выбрано больше
	// одного, явно отмечаем это, чтобы результат не приняли за сравнение всего кластера.
	sample := ""
	if len(r.targets) > 1 || len(otherShards) > 1 {
		sample = ui.Dim.Render("  (first-shard sample; \\diff compares a table across all current shards)")
	}
	// Проверяем доступность обеих сторон, чтобы недоступный шард был явно показан,
	// а не выглядел как пустая схема (что читалось бы как «нет различий»).
	if err := r.reachable(a); err != nil {
		return fmt.Errorf("cannot read %s: %v", aName, oneLine(err.Error()))
	}
	if err := r.reachable(b); err != nil {
		return fmt.Errorf("cannot read %s: %v", bName, oneLine(err.Error()))
	}
	fmt.Fprintf(r.out, "%s  %s ↔ %s%s\n", sevColor("compare"), aName, bName, sample)

	schemaA, schemaB := r.collectSchema(a), r.collectSchema(b)
	idxA, idxB := r.collectIndexes(a), r.collectIndexes(b)
	cfgA, cfgOKA := r.collectSettings(a)
	cfgB, cfgOKB := r.collectSettings(b)
	extA, extB := r.collectExtensions(a), r.collectExtensions(b)
	fnA, fnB := r.collectFunctions(a), r.collectFunctions(b)
	enA, enB := r.collectEnums(a), r.collectEnums(b)
	collA, collB := r.collectCollations(a), r.collectCollations(b)

	r.reportSchemaDiff(schemaA, schemaB)
	r.reportMapDiff("index differences", idxA, idxB, true)
	r.reportMapDiff("extension differences (name → version)", extA, extB, true)
	r.reportMapDiff("function differences (signature → volatility/security/body)", fnA, fnB, true)
	r.reportMapDiff("enum differences (type → labels)", enA, enB, true)
	r.reportMapDiff("collation differences (name → provider/locale)", collA, collB, true)
	r.reportSettingsDiff(cfgA, cfgB, cfgOKA && cfgOKB)
	return nil
}

// collectFunctions собирает пользовательские функции/процедуры с сигнатурой и
// «отпечатком поведения» (волатильность, SECURITY DEFINER, тип результата, хэш тела)
// — дрейф определения функции иначе невидим, хотя меняет поведение запросов.
func (r *REPL) collectFunctions(s cluster.Shard) map[string]string {
	out := map[string]string{}
	res, _ := r.docQuery(s, `SELECT n.nspname||'.'||p.proname||'('||pg_get_function_identity_arguments(p.oid)||')',
		CASE p.provolatile WHEN 'v' THEN 'volatile' WHEN 's' THEN 'stable' ELSE 'immutable' END
		|| CASE WHEN p.prosecdef THEN ' secdef' ELSE '' END
		|| ' -> ' || pg_get_function_result(p.oid)
		|| ' #' || substr(md5(coalesce(p.prosrc,'')), 1, 8)
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname NOT IN ('pg_catalog','information_schema')
		ORDER BY 1`)
	if res == nil {
		return out
	}
	for _, row := range res.Rows {
		out[cellStr(row, 0)] = cellStr(row, 1)
	}
	return out
}

// collectEnums собирает enum-типы и их метки в порядке сортировки — дрейф набора
// меток (или их порядка) между хранилищами часто ломает сравнения/ограничения.
func (r *REPL) collectEnums(s cluster.Shard) map[string]string {
	out := map[string]string{}
	res, _ := r.docQuery(s, `SELECT n.nspname||'.'||t.typname,
		string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder)
		FROM pg_enum e JOIN pg_type t ON t.oid = e.enumtypid
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname NOT IN ('pg_catalog','information_schema')
		GROUP BY 1 ORDER BY 1`)
	if res == nil {
		return out
	}
	for _, row := range res.Rows {
		out[cellStr(row, 0)] = cellStr(row, 1)
	}
	return out
}

// collectCollations собирает пользовательские коллации с провайдером и локалью —
// расхождение коллации/провайдера меняет порядок сортировки и сравнение строк.
func (r *REPL) collectCollations(s cluster.Shard) map[string]string {
	out := map[string]string{}
	// Только версионно-универсальные колонки: collprovider/collcollate/collctype есть
	// во всех поддерживаемых версиях; ICU-локаль (colliculocale/colllocale) меняла имя
	// между мажорами — её опускаем, чтобы запрос не падал на части серверов.
	res, _ := r.docQuery(s, `SELECT n.nspname||'.'||c.collname,
		c.collprovider::text || ' ' || coalesce(c.collcollate,'') ||
		CASE WHEN coalesce(c.collctype,'') <> coalesce(c.collcollate,'') THEN '/'||coalesce(c.collctype,'') ELSE '' END
		FROM pg_collation c JOIN pg_namespace n ON n.oid = c.collnamespace
		WHERE n.nspname NOT IN ('pg_catalog','information_schema')
		ORDER BY 1`)
	if res == nil {
		return out
	}
	for _, row := range res.Rows {
		out[cellStr(row, 0)] = cellStr(row, 1)
	}
	return out
}

func (r *REPL) collectSchema(s cluster.Shard) map[string]map[string]string {
	out := map[string]map[string]string{}
	res, _ := r.docQuery(s, `SELECT table_schema||'.'||table_name, column_name, data_type, is_nullable,
		coalesce(character_maximum_length::text,''),
		coalesce(numeric_precision::text,''), coalesce(numeric_scale::text,''),
		coalesce(datetime_precision::text,''), coalesce(interval_type,'')
		FROM information_schema.columns
		WHERE table_schema NOT IN ('pg_catalog','information_schema')
		ORDER BY 1,2`)
	if res == nil {
		return out
	}
	for _, row := range res.Rows {
		tbl := cellStr(row, 0) // с указанием схемы: public.users ≠ audit.users
		col := cellStr(row, 1)
		dataType := cellStr(row, 2)
		nullable := cellStr(row, 3)
		charLen := cellStr(row, 4)
		numPrec := cellStr(row, 5)
		numScale := cellStr(row, 6)
		dtPrec := cellStr(row, 7)
		intvlType := cellStr(row, 8)

		typ := dataType
		switch {
		case dataType == "numeric" && numPrec != "":
			typ += "(" + numPrec
			if numScale != "" {
				typ += "," + numScale
			}
			typ += ")"
		case charLen != "": // character varying/character/bit
			typ += "(" + charLen + ")"
		case dataType == "interval":
			// interval_type уже содержит точность долей секунды; добавляем точность
			// для голого interval только если она не дефолтная (6).
			if intvlType != "" {
				typ += " " + intvlType
			} else if dtPrec != "" && dtPrec != "6" {
				typ += "(" + dtPrec + ")"
			}
		case strings.Contains(dataType, "time") && dtPrec != "" && dtPrec != "6":
			// Каноничная форма: timestamp(3) with time zone — точность перед суффиксом
			// " with/without time zone"; дефолтная точность (6) опускается.
			if i := strings.Index(dataType, " with"); i >= 0 {
				typ = dataType[:i] + "(" + dtPrec + ")" + dataType[i:]
			} else {
				typ += "(" + dtPrec + ")"
			}
		}
		if nullable == "NO" {
			typ += " NOT NULL"
		}
		if out[tbl] == nil {
			out[tbl] = map[string]string{}
		}
		out[tbl][col] = typ
	}
	return out
}

func (r *REPL) collectIndexes(s cluster.Shard) map[string]string {
	out := map[string]string{}
	res, _ := r.docQuery(s, `SELECT schemaname||'.'||indexname, indexdef FROM pg_indexes
		WHERE schemaname NOT IN ('pg_catalog','information_schema')`)
	if res == nil {
		return out
	}
	for _, row := range res.Rows {
		// Ключ schema.index, чтобы одноимённые индексы в разных схемах
		// не перезаписывали друг друга.
		out[cellStr(row, 0)] = cellStr(row, 1)
	}
	return out
}

// collectExtensions собирает установленные расширения и их ВЕРСИИ — расхождение
// версий (или наличие/отсутствие) между хранилищами часто объясняет разное поведение.
func (r *REPL) collectExtensions(s cluster.Shard) map[string]string {
	out := map[string]string{}
	res, _ := r.docQuery(s, `SELECT extname, extversion FROM pg_extension ORDER BY extname`)
	if res == nil {
		return out
	}
	for _, row := range res.Rows {
		out[cellStr(row, 0)] = cellStr(row, 1)
	}
	return out
}

// collectSettings собирает значимые параметры pg_settings. Второй результат —
// флаг доступности: false означает НАСТОЯЩУЮ ошибку чтения (недоступная секция),
// а не «нет различий». Безобидный пропуск (нет прав/представления) docQuery
// классифицирует как ok=true с пустой картой, как и прочие collect*-функции.
func (r *REPL) collectSettings(s cluster.Shard) (map[string]string, bool) {
	out := map[string]string{}
	// Собираем литерал IN-списка из нужных параметров.
	quoted := make([]string, len(settingsOfInterest))
	for i, n := range settingsOfInterest {
		quoted[i] = "'" + n + "'"
	}
	// Нормализуем единицы: множители с цифрой (напр. "8kB") и размерные суффиксы
	// переводим в человекочитаемый размер, единицы времени дописываем как есть.
	sql := `SELECT name, CASE
		WHEN unit ~ '^[0-9]' THEN pg_size_pretty(setting::bigint * pg_size_bytes(unit))
		WHEN unit = ANY('{kB,MB,GB,TB}') THEN pg_size_pretty(pg_size_bytes(setting || unit))
		WHEN coalesce(unit,'') = '' THEN setting
		ELSE setting || unit END
		FROM pg_settings WHERE name IN (` + strings.Join(quoted, ",") + ")"
	res, isErr := r.docQuery(s, sql)
	if isErr {
		// Настоящая ошибка чтения: секция config недоступна, а не пустая.
		return out, false
	}
	if res == nil {
		return out, true
	}
	for _, row := range res.Rows {
		out[cellStr(row, 0)] = cellStr(row, 1)
	}
	return out, true
}

// reportSchemaDiff печатает различия таблиц и колонок.
func (r *REPL) reportSchemaDiff(a, b map[string]map[string]string) {
	var lines []string
	tables := unionKeys2(a, b)
	for _, t := range tables {
		ca, oka := a[t]
		cb, okb := b[t]
		switch {
		case oka && !okb:
			lines = append(lines, fmt.Sprintf("table %s: only in A", t))
		case !oka && okb:
			lines = append(lines, fmt.Sprintf("table %s: only in B", t))
		default:
			for _, col := range unionKeys(ca, cb) {
				va, oka := ca[col]
				vb, okb := cb[col]
				switch {
				case oka && !okb:
					lines = append(lines, fmt.Sprintf("%s.%s: only in A (%s)", t, col, va))
				case !oka && okb:
					lines = append(lines, fmt.Sprintf("%s.%s: only in B (%s)", t, col, vb))
				case va != vb:
					lines = append(lines, fmt.Sprintf("%s.%s: A %s / B %s", t, col, va, vb))
				}
			}
		}
	}
	printDiffSection(r, "schema differences", lines)
}

// reportMapDiff печатает различия наличия/определения для плоской карты name->def.
func (r *REPL) reportMapDiff(title string, a, b map[string]string, defAware bool) {
	var lines []string
	for _, k := range unionKeys(a, b) {
		va, oka := a[k]
		vb, okb := b[k]
		switch {
		case oka && !okb:
			lines = append(lines, fmt.Sprintf("%s: only in A", k))
		case !oka && okb:
			lines = append(lines, fmt.Sprintf("%s: only in B", k))
		case defAware && va != vb:
			// Короткие значения (например, версии расширений) показываем целиком;
			// длинные (определения индексов) — обобщённо, чтобы не залить вывод.
			if len(va) <= 60 && len(vb) <= 60 {
				lines = append(lines, fmt.Sprintf("%s: %s ↔ %s", k, va, vb))
			} else {
				lines = append(lines, fmt.Sprintf("%s: definition differs", k))
			}
		}
	}
	printDiffSection(r, title, lines)
}

func (r *REPL) reportSettingsDiff(a, b map[string]string, available bool) {
	// Недоступность pg_settings (нет прав/ошибка чтения) не должна выглядеть как
	// «config differences: none» — это маскирует реальную разницу. Явно сообщаем.
	if !available {
		fmt.Fprintln(r.out, "config differences: unavailable (could not read pg_settings on one or both sides)")
		return
	}
	var lines []string
	for _, n := range settingsOfInterest {
		va, oka := a[n]
		vb, okb := b[n]
		switch {
		case oka && !okb:
			// Параметр виден только на стороне A (на B недоступен/не выставлен) —
			// раньше молча терялся. Показываем по образцу reportMapDiff.
			lines = append(lines, fmt.Sprintf("%s: only in A (%s)", n, va))
		case !oka && okb:
			lines = append(lines, fmt.Sprintf("%s: only in B (%s)", n, vb))
		case oka && okb && va != vb:
			lines = append(lines, fmt.Sprintf("%s: A %s / B %s", n, va, vb))
		}
	}
	printDiffSection(r, "config differences", lines)
}

func printDiffSection(r *REPL, title string, lines []string) {
	if len(lines) == 0 {
		fmt.Fprintf(r.out, "%s: none\n", title)
		return
	}
	fmt.Fprintf(r.out, "%s (%d):\n", title, len(lines))
	limit := 30
	for i, l := range lines {
		if i >= limit {
			fmt.Fprintf(r.out, "  … and %d more\n", len(lines)-limit)
			break
		}
		fmt.Fprintf(r.out, "  %s\n", l)
	}
}

func unionKeys(a, b map[string]string) []string {
	set := map[string]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func unionKeys2(a, b map[string]map[string]string) []string {
	set := map[string]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
