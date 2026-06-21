// Package render форматирует результаты запросов в psql-подобные таблицы,
// включая объединённый многошардовый вид с дополнительной колонкой "shard".
package render

import (
	"database/sql/driver"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"

	"terox/internal/db"
	"terox/internal/ui"
)

// formatValue выводит значение одной ячейки в стиле psql. Некоторые типы pgx
// (numeric и другие без нативного Go-типа) приходят как структуры pgtype,
// реализующие driver.Valuer; такие разворачиваются в базовое значение, чтобы
// печаталось "EXTRACT(...) = 20", а не "{2 1 false finite true}".
func formatValue(v any) string {
	if v == nil {
		return "NULL"
	}
	switch t := v.(type) {
	case [16]byte:
		// pgx отдаёт uuid как массив байт; печатаем каноническим
		// 8-4-4-4-12, а не "[114 158 ...]".
		return formatUUID(t)
	case []byte:
		return string(t)
	case time.Time:
		return t.Format("2006-01-02 15:04:05.999999-07")
	case string:
		return t
	case float64:
		return formatFloat(t, 64)
	case float32:
		return formatFloat(float64(t), 32)
	}
	if vr, ok := v.(driver.Valuer); ok {
		if dv, err := vr.Value(); err == nil && dv != nil {
			if s, ok := dv.(string); ok {
				return trimNumeric(s) // numeric: 28.500000 -> 28.5
			}
			if _, again := dv.(driver.Valuer); !again {
				return formatValue(dv)
			}
		}
	}
	return fmt.Sprintf("%v", v)
}

// formatUUID форматирует 16-байтовый массив (как pgx отдаёт тип uuid) в
// каноническую строку 8-4-4-4-12, например "729e8a3e-b9ca-4182-9d9e-20d2b4304fb4".
func formatUUID(b [16]byte) string {
	const hex = "0123456789abcdef"
	var buf [36]byte
	j := 0
	for i := range 16 {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf[j] = '-'
			j++
		}
		buf[j] = hex[b[i]>>4]
		buf[j+1] = hex[b[i]&0x0f]
		j += 2
	}
	return string(buf[:])
}

// formatFloat повторяет psql: обычная десятичная запись для нормальных величин,
// экспонента только для очень больших/малых или нечисловых значений.
func formatFloat(f float64, bits int) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return strconv.FormatFloat(f, 'g', -1, bits)
	}
	if a := math.Abs(f); a != 0 && (a >= 1e15 || a < 1e-4) {
		return strconv.FormatFloat(f, 'g', -1, bits)
	}
	return strconv.FormatFloat(f, 'f', -1, bits)
}

// trimNumeric убирает хвостовые нули (и точку в конце) из десятичной строки,
// оставляя нечисловые строки без изменений.
func trimNumeric(s string) string {
	if !strings.Contains(s, ".") || strings.ContainsAny(s, "eE") {
		return s
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && c != '.' && c != '-' && c != '+' {
			return s // не простое десятичное — оставляем как есть
		}
	}
	t := strings.TrimRight(s, "0")
	t = strings.TrimRight(t, ".")
	if t == "" || t == "-" || t == "+" {
		return "0"
	}
	return t
}

func newWriter(out io.Writer) table.Writer {
	tw := table.NewWriter()
	tw.SetOutputMirror(out)
	tw.SetStyle(table.StyleLight)
	tw.Style().Options.SeparateRows = false
	// Имена колонок сохраняются как есть (psql-стиль), без перевода в верхний регистр.
	tw.Style().Format.Header = text.FormatDefault
	tw.Style().Format.Footer = text.FormatDefault
	if ui.Enabled {
		tw.Style().Color.Header = text.Colors{text.FgGreen}
		tw.Style().Color.Border = text.Colors{text.FgHiBlack}
		tw.Style().Color.Separator = text.Colors{text.FgHiBlack}
	}
	return tw
}

// shardColumnConfig красит ведущую provenance-колонку (name) в зелёный на TTY.
func shardColumnConfig(name string) []table.ColumnConfig {
	if !ui.Enabled {
		return nil
	}
	return []table.ColumnConfig{{Name: name, Colors: text.Colors{text.FgGreen}}}
}

// paint применяет цвет только в интерактивном терминале.
func paint(c text.Color, s string) string {
	if ui.Enabled {
		return c.Sprint(s)
	}
	return s
}

// durStr выводит суффикс времени (", 5ms") при включённом timing, иначе "".
// Управляется через \timing.
func durStr(timing bool, d time.Duration) string {
	if !timing {
		return ""
	}
	return ", " + fmtDur(d)
}

// Single выводит результат одного шарда. maxRows <= 0 — без ограничения.
func Single(out io.Writer, res *db.Result, maxRows int, timing bool) {
	if res == nil {
		return
	}
	if !res.IsSelect {
		fmt.Fprintf(out, "OK (%d rows affected%s)\n", res.RowsAffected, durStr(timing, res.Duration))
		return
	}

	tw := newWriter(out)
	header := make(table.Row, len(res.Columns))
	for i, c := range res.Columns {
		header[i] = c
	}
	tw.AppendHeader(header)

	shown := 0
	for _, row := range res.Rows {
		if maxRows > 0 && shown >= maxRows {
			break
		}
		r := make(table.Row, len(res.Columns))
		for i := range r {
			if i < len(row) {
				r[i] = formatValue(row[i])
			} else {
				r[i] = formatValue(nil)
			}
		}
		tw.AppendRow(r)
		shown++
	}
	tw.Render()

	total := len(res.Rows)
	switch {
	case res.Truncated:
		// Строки обрезаны при материализации (защита памяти): точное
		// количество неизвестно, на сервере есть ещё.
		fmt.Fprintf(out, "(showing first %d; more rows exist — capped at \\maxrows; \\export writes the full result%s)\n", shown, durStr(timing, res.Duration))
	case maxRows > 0 && total > maxRows:
		fmt.Fprintf(out, "(%d rows, showing %d; raise with \\maxrows%s)\n", total, maxRows, durStr(timing, res.Duration))
	default:
		fmt.Fprintf(out, "(%d rows%s)\n", total, durStr(timing, res.Duration))
	}
}

// Multi объединяет результаты по шардам в одну таблицу с ведущей колонкой
// "shard". Наборы колонок шардов объединяются; отсутствующие ячейки — NULL.
// Шарды с ошибками перечислены под таблицей.
func Multi(out io.Writer, results []db.ShardResult, maxRows int) {
	var okResults []db.ShardResult
	var errResults []db.ShardResult
	var selectResults []db.ShardResult // успешные шарды, вернувшие набор строк
	var inputs [][]string              // колонки каждого SELECT-результата (в порядке)
	totalRows := 0

	for _, sr := range results {
		if sr.Err != nil {
			errResults = append(errResults, sr)
			continue
		}
		if sr.Result == nil {
			continue
		}
		okResults = append(okResults, sr)
		if sr.Result.IsSelect {
			selectResults = append(selectResults, sr)
			inputs = append(inputs, sr.Result.Columns)
			totalRows += len(sr.Result.Rows)
		}
	}

	// Объединение колонок с MULTISET-семантикой (общий мерджер с Merge), чтобы
	// повторяющиеся имена (SELECT id, id) не сворачивались в одну колонку.
	unionCols, slots := unionMultiset(inputs)
	selectShards := len(selectResults)
	anySelect := selectShards > 0

	if anySelect {
		prov := provenanceColName(unionCols)
		tw := newWriter(out)
		tw.SetColumnConfigs(shardColumnConfig(prov))
		header := make(table.Row, 0, len(unionCols)+1)
		header = append(header, prov)
		for _, c := range unionCols {
			header = append(header, c)
		}
		tw.AppendHeader(header)

		shown := 0
		truncated := false
		for k, sr := range selectResults {
			colSlot := slots[k] // позиция i-й колонки этого шарда в объединении
			for _, row := range sr.Result.Rows {
				if maxRows > 0 && shown >= maxRows {
					truncated = true
					break
				}
				r := make(table.Row, len(unionCols)+1)
				r[0] = sr.Shard.LabelDB()
				for i := 1; i < len(r); i++ {
					r[i] = "NULL" // отсутствующая у этого шарда колонка
				}
				for i := range sr.Result.Columns {
					if i < len(colSlot) && colSlot[i] >= 0 && i < len(row) {
						r[colSlot[i]+1] = formatValue(row[i]) // +1 на ведущую provenance-колонку
					}
				}
				tw.AppendRow(r)
				shown++
			}
			if truncated {
				break
			}
		}
		tw.Render()

		// Если у шарда строки обрезаны при материализации, общий итог тоже
		// неизвестен; любую обрезку считаем "неполным результатом".
		dbTruncated := false
		for _, sr := range okResults {
			if sr.Result != nil && sr.Result.Truncated {
				dbTruncated = true
				break
			}
		}
		switch {
		case dbTruncated:
			fmt.Fprintf(out, "(showing %d row(s) across %d shards — result truncated; \\export writes the full set, or raise \\maxrows)\n", shown, selectShards)
		case truncated:
			fmt.Fprintf(out, "(%d rows across %d shards, showing %d; raise with \\maxrows)\n",
				totalRows, selectShards, maxRows)
		default:
			fmt.Fprintf(out, "(%d rows across %d shards)\n", totalRows, selectShards)
		}
	} else if len(okResults) > 0 {
		// Все команды (не SELECT): суммируем затронутые строки по шардам.
		var affected int64
		for _, sr := range okResults {
			if sr.Result != nil {
				affected += sr.Result.RowsAffected
			}
		}
		fmt.Fprintf(out, "OK on %d shards (%d rows affected total)\n", len(okResults), affected)
	}

	// Дрейф типов: одноимённая колонка с разными типами на разных шардах.
	for _, w := range DetectTypeDrift(results) {
		fmt.Fprintf(out, "%s %s\n", paint(text.FgYellow, "⚠"), w)
	}

	if len(errResults) > 0 {
		sort.SliceStable(errResults, func(i, j int) bool {
			return errResults[i].Shard.Position < errResults[j].Shard.Position
		})
		fmt.Fprintf(out, "\n%d shard(s) failed:\n", len(errResults))
		for _, sr := range errResults {
			fmt.Fprintf(out, "  %s %s: %v\n",
				paint(text.FgRed, "✗"), sr.Shard.LabelDB(), errDetail(sr.Err))
		}
	}
}

// Merge сводит результаты SELECT по шардам в один набор (колонки, строки) с
// ведущей provenance-колонкой и сырыми значениями (nil для отсутствующих ячеек),
// для экспорта. Не-SELECT результаты игнорируются. Использует тот же мерджер
// схемы (unionMultiset/provenanceColName), что и табличный вывод Multi, поэтому
// table, vertical, CSV, JSON и сохранённый результат видят одинаковые колонки.
func Merge(results []db.ShardResult) ([]string, [][]any) {
	var selectResults []db.ShardResult
	var inputs [][]string
	for _, sr := range results {
		if sr.Err != nil || sr.Result == nil || !sr.Result.IsSelect {
			continue
		}
		selectResults = append(selectResults, sr)
		inputs = append(inputs, sr.Result.Columns)
	}
	unionCols, slots := unionMultiset(inputs)
	cols := append([]string{provenanceColName(unionCols)}, unionCols...)

	var rows [][]any
	for k, sr := range selectResults {
		colSlot := slots[k]
		for _, row := range sr.Result.Rows {
			out := make([]any, len(cols))
			out[0] = sr.Shard.LabelDB()
			for i := range sr.Result.Columns {
				if i < len(colSlot) && colSlot[i] >= 0 && i < len(row) {
					out[colSlot[i]+1] = row[i] // +1 на ведущую provenance-колонку
				}
			}
			rows = append(rows, out)
		}
	}
	return cols, rows
}

// unionMultiset вычисляет объединение имён колонок нескольких результатов с
// MULTISET-семантикой: имя, общее для входов, сворачивается в одну колонку, но
// имя, повторяющееся ВНУТРИ одного входа (SELECT id, id), сохраняет каждое
// вхождение — берётся максимальная кратность имени по входам. Порядок — по
// первому появлению, все вхождения имени сгруппированы у его первого появления.
// Возвращает имена колонок объединения и для каждого входа срез slot-индексов:
// slots[k][i] — позиция i-й колонки k-го входа в unionCols.
func unionMultiset(inputs [][]string) (unionCols []string, slots [][]int) {
	maxCount := map[string]int{}
	var nameOrder []string
	for _, cols := range inputs {
		cnt := map[string]int{}
		for _, c := range cols {
			if _, seen := maxCount[c]; !seen && cnt[c] == 0 {
				nameOrder = append(nameOrder, c)
			}
			cnt[c]++
		}
		for c, k := range cnt {
			if k > maxCount[c] {
				maxCount[c] = k
			}
		}
	}
	unionSlots := map[string][]int{}
	for _, c := range nameOrder {
		for k := 0; k < maxCount[c]; k++ {
			unionSlots[c] = append(unionSlots[c], len(unionCols))
			unionCols = append(unionCols, c)
		}
	}
	slots = make([][]int, len(inputs))
	for ii, cols := range inputs {
		s := make([]int, len(cols))
		occ := map[string]int{}
		for i, c := range cols {
			k := occ[c]
			occ[c]++
			if sl := unionSlots[c]; k < len(sl) {
				s[i] = sl[k]
			} else {
				s[i] = -1
			}
		}
		slots[ii] = s
	}
	return unionCols, slots
}

// colSig — сигнатура типа колонки: базовый OID плюс type modifier (-1 = нет),
// чтобы дрейф различал и varchar(10)/varchar(255) при общем OID.
type colSig struct {
	oid uint32
	mod int32
}

// SortMerged глобально сортирует объединённые строки шардов (из Merge) по
// колонке col — то, чего per-shard ORDER BY не даёт: на каждом шарде свой
// порядок, а глобального нет. Сравнение типо-осведомлённое: числа (в т.ч.
// numeric/целые в виде строк) сравниваются численно и идут перед строками,
// NULL — всегда последними; desc разворачивает порядок значений. col ищется
// среди cols (включая ведущую provenance-колонку); при отсутствии — ошибка.
// Сортировка стабильная. Изменяет rows на месте.
func SortMerged(cols []string, rows [][]any, col string, desc bool) error {
	idx := -1
	for i, c := range cols {
		if c == col {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("--order-by: column %q not found (available: %s)", col, strings.Join(cols, ", "))
	}
	sort.SliceStable(rows, func(i, j int) bool {
		var vi, vj any
		if idx < len(rows[i]) {
			vi = rows[i][idx]
		}
		if idx < len(rows[j]) {
			vj = rows[j][idx]
		}
		ki, fi, si := sortKey(vi)
		kj, fj, sj := sortKey(vj)
		if ki != kj {
			return ki < kj // числа (0) < строки (1) < NULL (2); NULLS LAST в обоих направлениях
		}
		switch ki {
		case 0:
			if fi == fj {
				return false
			}
			if desc {
				return fi > fj
			}
			return fi < fj
		case 1:
			if si == sj {
				return false
			}
			if desc {
				return si > sj
			}
			return si < sj
		default:
			return false // оба NULL — равны (стабильно)
		}
	})
	return nil
}

// sortKey возвращает (вид, число, строка) для сравнения: вид 0 — числовое
// значение (включая numeric/целые в строковом виде), 1 — строка, 2 — NULL.
func sortKey(v any) (int, float64, string) {
	if v == nil {
		return 2, 0, ""
	}
	if f, ok := numericOf(v); ok {
		return 0, f, ""
	}
	return 1, 0, formatValue(v)
}

// numericOf возвращает числовое значение v (true), если оно число или numeric/
// целое в строковом виде; иначе (0,false). Общий для сортировки и агрегации.
// Нативные float NaN/Inf — это числа и проходят; но ТЕКСТ "Inf"/"NaN"/"Infinity"
// и Go-литералы с подчёркиваниями ("1_000") — это строки, а не числа (ParseFloat
// их принял бы), поэтому в строковой ветке они отвергаются.
func numericOf(v any) (float64, bool) {
	switch t := v.(type) {
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case float64:
		return t, true
	case float32:
		return float64(t), true
	}
	s := strings.TrimSpace(formatValue(v))
	if strings.ContainsRune(s, '_') {
		return 0, false
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return f, true
	}
	return 0, false
}

// integerOf возвращает целое значение v (true), если это целочисленный тип или
// целое в строковом виде (pg int8 -> int64, numeric "100" -> 100). Float и
// дробные строки целыми не считаются. Нужен Aggregate, чтобы суммировать целые
// БЕЗ потери точности float64 (>2^53) и без научной записи.
func integerOf(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	}
	s := strings.TrimSpace(formatValue(v))
	if strings.ContainsRune(s, '_') {
		return 0, false
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i, true
	}
	return 0, false
}

// Aggregate сворачивает ВСЕ строки (по всем шардам) в одну: числовые колонки
// суммируются (per-shard count()/sum() -> глобальный итог); нечисловые показывают
// общее значение, если все строки совпали, иначе пусто. Ведущая provenance-
// колонка (shard) отбрасывается. Чистая функция.
func Aggregate(cols []string, rows [][]any) ([]string, [][]any) {
	if len(cols) <= 1 {
		return cols, rows
	}
	outCols := append([]string(nil), cols[1:]...)
	if len(rows) == 0 {
		return outCols, nil // нет строк — нечего сворачивать (а не строка из NULL)
	}
	n := len(outCols)
	fsum := make([]float64, n) // дробная сумма (запасной аккумулятор)
	isum := make([]int64, n)   // целочисленная сумма (точна для int8 и т.п.)
	allNum := make([]bool, n)  // все значения колонки числовые
	allInt := make([]bool, n)  // все числовые значения целые и без переполнения
	anyVal := make([]bool, n)
	common := make([]string, n)
	haveCommon := make([]bool, n)
	mixed := make([]bool, n)
	for i := range allNum {
		allNum[i], allInt[i] = true, true
	}
	for _, row := range rows {
		for i := 0; i < n; i++ {
			ci := i + 1 // пропускаем provenance-колонку
			if ci >= len(row) || row[ci] == nil {
				continue
			}
			v := row[ci]
			anyVal[i] = true
			if f, ok := numericOf(v); ok {
				fsum[i] += f
				if iv, isInt := integerOf(v); isInt && allInt[i] {
					if s := isum[i] + iv; (iv > 0) == (s > isum[i]) || iv == 0 {
						isum[i] = s
					} else {
						allInt[i] = false // переполнение int64 — переходим на float
					}
				} else if !isInt {
					allInt[i] = false
				}
			} else {
				allNum[i] = false
			}
			s := formatValue(v)
			if !haveCommon[i] {
				common[i], haveCommon[i] = s, true
			} else if common[i] != s {
				mixed[i] = true
			}
		}
	}
	out := make([]any, n)
	for i := 0; i < n; i++ {
		switch {
		case !anyVal[i]:
			out[i] = nil
		case allNum[i] && allInt[i]:
			out[i] = isum[i] // точная целая сумма, печатается без научной записи
		case allNum[i]:
			out[i] = fsum[i]
		case !mixed[i]:
			out[i] = common[i]
		default:
			out[i] = ""
		}
	}
	return outCols, [][]any{out}
}

// DetectTypeDrift возвращает предупреждения о колонках, чей тип РАЗЛИЧАЕТСЯ между
// шардами (например, id как int4 на одном шарде и int8 на другом, либо
// varchar(10) против varchar(255)). Это семантический дрейф схемы, который
// объединённый вывод скрыл бы. Сравнение идёт по (имя, номер вхождения В ПРЕДЕЛАХ
// шарда), поэтому повторяющиеся одноимённые колонки одного результата
// (SELECT a AS id, b AS id) НЕ выдаются за межшардовый дрейф. Колонки без снятого
// типа (ColTypes пуст или OID=0) пропускаются. Чистая функция; порядок
// предупреждений — по первому появлению колонки.
func DetectTypeDrift(results []db.ShardResult) []string {
	sigsByCol := map[string]map[colSig]bool{}
	var order []string
	for _, sr := range results {
		if sr.Err != nil || sr.Result == nil || !sr.Result.IsSelect {
			continue
		}
		occ := map[string]int{}
		for i, name := range sr.Result.Columns {
			k := occ[name]
			occ[name]++
			if i >= len(sr.Result.ColTypes) {
				continue
			}
			oid := sr.Result.ColTypes[i]
			if oid == 0 {
				continue // неизвестный тип (например literal NULL) — сравнивать нечем
			}
			mod := int32(-1)
			if i < len(sr.Result.ColMods) {
				mod = sr.Result.ColMods[i]
			}
			key := fmt.Sprintf("%s#%d", name, k)
			m := sigsByCol[key]
			if m == nil {
				m = map[colSig]bool{}
				sigsByCol[key] = m
				order = append(order, key)
			}
			m[colSig{oid, mod}] = true
		}
	}
	var warns []string
	for _, key := range order {
		sigs := sigsByCol[key]
		if len(sigs) <= 1 {
			continue
		}
		name := key[:strings.LastIndexByte(key, '#')]
		warns = append(warns, fmt.Sprintf("column %q has differing types across shards (%s)", name, formatSigs(sigs)))
	}
	return warns
}

// formatSigs форматирует множество сигнатур типа в стабильную строку человекочитаемых
// имён типов, напр. "int8, int4" или "varchar(10), varchar(255)" (Feature 3: имена
// вместо сырых OID). Порядок детерминирован (по oid, затем typmod).
func formatSigs(set map[colSig]bool) string {
	list := make([]colSig, 0, len(set))
	for s := range set {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].oid != list[j].oid {
			return list[i].oid < list[j].oid
		}
		return list[i].mod < list[j].mod
	})
	parts := make([]string, len(list))
	for i, s := range list {
		parts[i] = db.TypeName(s.oid, s.mod)
	}
	return strings.Join(parts, ", ")
}

// MergedSchema возвращает типизированную схему (параллельно mergedCols), беря тип
// каждой колонки из ПЕРВОГО шарда, который её предоставил (по паре имя+вхождение).
// Колонка, которой нет ни в одном результате шарда (искусственная provenance-колонка
// "shard"/"_terox_shard", добавленная Merge), помечается Synthetic с типом text.
// Чистая функция — тестируется без БД (Feature 3).
func MergedSchema(results []db.ShardResult, mergedCols []string) []db.Column {
	seen := map[string]colSig{}
	for _, sr := range results {
		if sr.Err != nil || sr.Result == nil {
			continue
		}
		occ := map[string]int{}
		for i, name := range sr.Result.Columns {
			k := occ[name]
			occ[name]++
			key := fmt.Sprintf("%s#%d", name, k)
			if _, ok := seen[key]; ok {
				continue
			}
			var oid uint32
			if i < len(sr.Result.ColTypes) {
				oid = sr.Result.ColTypes[i]
			}
			mod := int32(-1)
			if i < len(sr.Result.ColMods) {
				mod = sr.Result.ColMods[i]
			}
			seen[key] = colSig{oid, mod}
		}
	}
	out := make([]db.Column, len(mergedCols))
	occ := map[string]int{}
	for i, name := range mergedCols {
		k := occ[name]
		occ[name]++
		key := fmt.Sprintf("%s#%d", name, k)
		if sig, ok := seen[key]; ok {
			out[i] = db.Column{Name: name, DataTypeOID: sig.oid, TypeModifier: sig.mod, TypeName: db.TypeName(sig.oid, sig.mod), Occurrence: k}
		} else {
			// Не найдена ни в одном шарде → искусственная provenance-колонка.
			out[i] = db.Column{Name: name, TypeName: "text", Occurrence: k, Synthetic: true}
		}
	}
	return out
}

// SchemaCheckStatus сообщает полноту проверки типов объединённой схемы:
//   - "skipped" — тип не снят ни у одной (не-synthetic) колонки (export-путь,
//     не-SELECT) → дрейф НЕ проверялся;
//   - "partial" — у части колонок тип неизвестен (OID=0, например literal NULL);
//   - "complete" — у всех реальных колонок тип известен.
//
// Позволяет потребителю отличить «дрейфа нет» от «не проверяли».
func SchemaCheckStatus(schema []db.Column) string {
	real, typed, unknown := 0, 0, 0
	for _, c := range schema {
		if c.Synthetic {
			continue
		}
		real++
		if c.DataTypeOID == 0 {
			unknown++
		} else {
			typed++
		}
	}
	switch {
	case real == 0 || typed == 0:
		return "skipped"
	case unknown > 0:
		return "partial"
	default:
		return "complete"
	}
}

// provenanceColName возвращает имя искусственной ведущей колонки шарда,
// гарантированно не совпадающее с колонкой пользователя: обычно "shard", но если
// результат сам содержит колонку "shard", используется зарезервированное имя
// "_terox_shard" (с добавлением подчёркиваний при дальнейшем конфликте), чтобы
// заголовок оставался однозначным.
func provenanceColName(unionCols []string) string {
	taken := make(map[string]bool, len(unionCols))
	for _, c := range unionCols {
		taken[c] = true
	}
	if !taken["shard"] {
		return "shard"
	}
	name := "_terox_shard"
	for taken[name] {
		name += "_"
	}
	return name
}

// Table выводит произвольную таблицу (используется \count, \locate, \diff,
// \ping). Если первый заголовок — "shard", эта колонка красится в зелёный.
func Table(out io.Writer, headers []string, rows [][]string, footer string) {
	tw := newWriter(out)
	if len(headers) > 0 && headers[0] == "shard" {
		tw.SetColumnConfigs(shardColumnConfig("shard"))
	}
	hr := make(table.Row, len(headers))
	for i, h := range headers {
		hr[i] = h
	}
	tw.AppendHeader(hr)
	for _, row := range rows {
		rr := make(table.Row, len(row))
		for i, c := range row {
			rr[i] = c
		}
		tw.AppendRow(rr)
	}
	tw.Render()
	if footer != "" {
		fmt.Fprintln(out, footer)
	}
}

// AnyTable рендерит произвольные (cols, rows) из Merge — например, после
// глобальной сортировки SortMerged — таблицей. Ведущая provenance-колонка
// (shard/_terox_shard) красится как обычно.
func AnyTable(out io.Writer, cols []string, rows [][]any, footer string) {
	tw := newWriter(out)
	if len(cols) > 0 && (cols[0] == "shard" || cols[0] == "_terox_shard") {
		tw.SetColumnConfigs(shardColumnConfig(cols[0]))
	}
	hr := make(table.Row, len(cols))
	for i, c := range cols {
		hr[i] = c
	}
	tw.AppendHeader(hr)
	for _, row := range rows {
		rr := make(table.Row, len(cols))
		for i := range cols {
			if i < len(row) {
				rr[i] = formatValue(row[i])
			} else {
				rr[i] = formatValue(nil)
			}
		}
		tw.AppendRow(rr)
	}
	tw.Render()
	if footer != "" {
		fmt.Fprintln(out, footer)
	}
}

// Vertical выводит результат одного шарда в развёрнутом виде (запись на строку),
// как \x в psql, для широких строк.
func Vertical(out io.Writer, res *db.Result, maxRows int, timing bool) {
	if res == nil {
		return
	}
	if !res.IsSelect {
		fmt.Fprintf(out, "OK (%d rows affected%s)\n", res.RowsAffected, durStr(timing, res.Duration))
		return
	}
	width := 0
	for _, c := range res.Columns {
		if len(c) > width {
			width = len(c)
		}
	}
	shown := 0
	for i, row := range res.Rows {
		if maxRows > 0 && shown >= maxRows {
			break
		}
		fmt.Fprintf(out, "%s[ row %d ]%s\n", paint(text.FgHiBlack, "-"), i+1, paint(text.FgHiBlack, strings.Repeat("-", 20)))
		for j, c := range res.Columns {
			var v any
			if j < len(row) {
				v = row[j]
			}
			fmt.Fprintf(out, "%-*s | %s\n", width, paint(text.FgGreen, c), formatValue(v))
		}
		shown++
	}
	switch {
	case res.Truncated:
		// Цикл мог остановиться раньше len(res.Rows) на локальном maxRows
		// (shown == maxRows < len(res.Rows)); печатаем фактически показанное,
		// как и Single, иначе счётчик соврёт о числе строк на экране.
		fmt.Fprintf(out, "(showing first %d; more rows exist — capped at \\maxrows; \\export writes the full result%s)\n", shown, durStr(timing, res.Duration))
	case maxRows > 0 && len(res.Rows) > maxRows:
		// Локальный лимит \maxrows: показали maxRows из большего набора — сообщаем
		// явно, как и Single, иначе полный счётчик выглядел бы как «показали всё».
		fmt.Fprintf(out, "(%d rows, showing %d; raise with \\maxrows%s)\n", len(res.Rows), maxRows, durStr(timing, res.Duration))
	default:
		fmt.Fprintf(out, "(%d rows%s)\n", len(res.Rows), durStr(timing, res.Duration))
	}
}

// WriteSingle выводит итог записи/миграции на одном шарде.
func WriteSingle(out io.Writer, res db.ExecResult) {
	if res.Err != nil {
		fmt.Fprintf(out, "%s %s: %v (%s)\n",
			paint(text.FgRed, "FAIL"), res.Shard.LabelDB(), errDetail(res.Err), fmtDur(res.Duration))
		return
	}
	fmt.Fprintf(out, "%s %s — %d rows affected (%s)\n",
		paint(text.FgGreen, "OK"), res.Shard.LabelDB(), res.Affected, fmtDur(res.Duration))
}

// ExecResults выводит статус записи/миграции по каждому шарду таблицей
// (shard, status, rows, time), затем сводку и ошибки.
func ExecResults(out io.Writer, results []db.ExecResult) {
	tw := newWriter(out)
	tw.SetColumnConfigs(shardColumnConfig("shard"))
	tw.AppendHeader(table.Row{"shard", "status", "rows", "time"})

	ok, failed := 0, 0
	var errs []db.ExecResult
	for _, r := range results {
		status := paint(text.FgGreen, "OK")
		rows := fmt.Sprintf("%d", r.Affected)
		if r.Err != nil {
			status = paint(text.FgRed, "FAIL")
			rows = "-"
			failed++
			errs = append(errs, r)
		} else {
			ok++
		}
		tw.AppendRow(table.Row{r.Shard.LabelDB(), status, rows, fmtDur(r.Duration)})
	}
	tw.Render()
	fmt.Fprintf(out, "summary: %d OK, %d failed (of %d)\n", ok, failed, len(results))

	for _, e := range errs {
		fmt.Fprintf(out, "  %s %s: %v\n",
			paint(text.FgRed, "✗"), e.Shard.LabelDB(), errDetail(e.Err))
	}
}

func oneLine(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
}

// errDetail рисует ошибку шарда с её SQLSTATE впереди, если это ошибка сервера
// PostgreSQL ("[23505] duplicate key…"), чтобы код состояния был виден отдельно
// от текста. Берёт ЧИСТОЕ сообщение из ClassifyError (pgErr.Message), а не
// err.Error() — иначе в строке дублировался бы "(SQLSTATE …)" и многословный
// префикс pgx. Для сетевых/клиентских ошибок (без SQLSTATE) — просто текст.
func errDetail(err error) string {
	info := db.ClassifyError(err)
	if info == nil {
		return ""
	}
	if info.SQLState != "" {
		return "[" + info.SQLState + "] " + oneLine(info.Message)
	}
	return oneLine(info.Message)
}

func fmtDur(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}
