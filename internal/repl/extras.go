package repl

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"terox/internal/db"
	"terox/internal/execution"
	"terox/internal/export"
	"terox/internal/sqlsplit"
)

// doExport пишет последний результат чтения в CSV или JSON. Если результат в
// памяти усечён (широкий SELECT, ограниченный ради памяти), запрос
// перезапускается и стримится целиком, а не пишется усечённый срез.
func (r *REPL) doExport(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: \\export csv|json <file>")
	}
	format := strings.ToLower(args[0])
	path := args[1]
	if len(r.lastCols) == 0 {
		return fmt.Errorf("nothing to export — run a SELECT first")
	}
	// Проверяем формат ДО создания файла, чтобы `\export typo out.csv`
	// не обрезал существующий out.csv перед ошибкой.
	if format != "csv" && format != "json" {
		return fmt.Errorf("unknown format %q (use csv or json)", format)
	}

	// Усечённый результат: в кэше лишь срез строк — стримим весь набор заново.
	if r.lastTruncated {
		return r.exportStreaming(format, path)
	}

	// Атомарная запись: неудачная/частичная кодировка не должна обрезать или
	// портить существующий файл назначения.
	err := writeFileAtomically(path, func(w io.Writer) error {
		switch format {
		case "csv":
			return export.WriteCSV(w, r.lastCols, r.lastRows)
		default:
			return export.WriteJSON(w, r.lastCols, r.lastRows)
		}
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(r.out, "exported %d rows to %s\n", len(r.lastRows), path)
	return nil
}

// writeFileAtomically пишет через временный файл в каталоге назначения и при
// успехе переименовывает его на место, удаляя временный файл при любой ошибке,
// чтобы неудачная запись не оставляла обрезанный или частичный файл.
func writeFileAtomically(path string, write func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			os.Remove(tmpName)
		}
	}()
	if err := write(tmp); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}

// exportStreaming перезапускает последний запрос и стримит весь результат в файл
// без материализации; используется, когда показанный результат был усечён.
// Объединённый заголовок (r.lastCols — "shard"+union-колонки при чтении с
// нескольких шардов) — каноническая схема; колонки каждого шарда сопоставляются
// с ним по имени.
func (r *REPL) exportStreaming(format, path string) error {
	query := strings.TrimSpace(r.lastQuery)
	if query == "" {
		return fmt.Errorf("cannot re-stream export: no query recorded")
	}
	// Повторный запуск записи изменил бы данные; read-only транзакция в StreamRead
	// — жёсткая защита, а это ранний дружелюбный отказ.
	if execution.IsWrite(query) {
		return fmt.Errorf("last query is not a plain read; refusing to re-run it for export")
	}
	if len(r.targets) == 0 {
		return fmt.Errorf("no shard selected")
	}

	// Стримим против ТЕХ ЖЕ целей, из которых пришёл результат, а не текущих:
	// смена сервиса/хранилища/шарда не должна перенаправить экспорт на другой
	// кластер.
	targets := r.lastTargets
	if len(targets) == 0 {
		targets = r.targets
	}
	if len(targets) == 0 {
		return fmt.Errorf("no shard selected")
	}

	header := r.lastCols
	// render.Merge добавляет синтетическую первую колонку "shard" только при
	// чтении с нескольких шардов, поэтому ориентируемся на число целей, а не на
	// имя в заголовке, которое настоящая колонка с именем "shard" могла бы
	// подделать.
	multi := len(targets) > 1
	dataStart := 0
	if multi {
		dataStart = 1
	}
	// Слоты по порядку вхождений: k-я колонка с именем c на шарде ложится в k-й
	// слот заголовка с именем c, чтобы дубликаты имён (SELECT id, id) не
	// схлопывались в одну позицию.
	headerSlots := make(map[string][]int, len(header))
	for i := dataStart; i < len(header); i++ {
		headerSlots[header[i]] = append(headerSlots[header[i]], i)
	}

	// Ограничиваем перезапуск серверным statement_timeout и даём Ctrl-C прервать
	// его.
	r.mgr.SetReadTimeout(r.stmtTimeout)
	defer r.mgr.SetReadTimeout("")
	ctx, cancel := interruptible()
	defer cancel()

	// Стримим во временный файл в каталоге назначения и переименовываем при
	// успехе, чтобы таймаут/ошибка шарда не обрезали и не оставляли частичный файл.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			os.Remove(tmpName)
		}
	}()

	var sw export.RowStreamer
	switch format {
	case "csv":
		sw, err = export.NewCSVStream(tmp, header)
	case "json":
		sw, err = export.NewJSONStream(tmp, header)
	}
	if err != nil {
		return err
	}

	for _, shard := range targets {
		label := shard.LabelDB()
		var colPos []int // колонка шарда i -> позиция в заголовке (-1 = отбросить)
		streamErr := r.mgr.StreamRead(ctx, shard, query,
			func(schema []db.Column) {
				colPos = make([]int, len(schema))
				occ := make(map[string]int, len(schema))
				for i, col := range schema {
					c := col.Name
					k := occ[c]
					occ[c]++
					if slots, ok := headerSlots[c]; ok && k < len(slots) {
						colPos[i] = slots[k]
					} else {
						colPos[i] = -1
					}
				}
			},
			func(row []any) error {
				out := make([]any, len(header))
				if multi {
					out[0] = label // первая колонка "shard"
				}
				for i, v := range row {
					if i < len(colPos) && colPos[i] >= 0 {
						out[colPos[i]] = v
					}
				}
				return sw.WriteRow(out)
			})
		if streamErr != nil {
			_ = sw.Close()
			return fmt.Errorf("export streaming on shard %s: %v", label, oneLine(streamErr.Error()))
		}
	}
	if err := sw.Close(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	fmt.Fprintf(r.out, "exported %d rows to %s (full result, re-streamed)\n", sw.Rows(), path)
	return nil
}

// doSave сохраняет именованный запрос: "\save name [sql...]" (без sql => последний
// запрос). Хвост SQL берётся дословно из исходной строки, чтобы сохранить точные
// пробелы строкового литерала (strings.Fields + склейка одним пробелом схлопнули
// бы 'a  b' в 'a b').
func (r *REPL) doSave(args []string, raw string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: \\save <name> [sql] (omit sql to save the last query)")
	}
	name := args[0]
	sql := strings.TrimSpace(rawTail(raw, 2)) // отбрасываем "\save" и "<name>"
	if sql == "" {
		sql = r.lastQuery
	}
	if sql == "" {
		return fmt.Errorf("no SQL to save")
	}
	if err := r.queries.Set(name, sql); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "saved %q\n", name)
	return nil
}

// doRun выполняет сохранённый запрос (подставляя любые :name параметры) через
// обычную проверку чтения/записи.
func (r *REPL) doRun(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: \\run <name>")
	}
	sql, ok := r.queries.Get(args[0])
	if !ok {
		return fmt.Errorf("no saved query %q (see \\queries)", args[0])
	}
	bound, err := r.bindParams(sql)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.out, "%s\n", bound) // показываем итоговый SQL перед запуском
	r.runStatement(strings.TrimSuffix(strings.TrimSpace(bound), ";"))
	return nil
}

var (
	paramRe  = regexp.MustCompile(`:[a-zA-Z_][a-zA-Z0-9_]*`)
	numLitRe = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
)

// bindParams подставляет :name параметры в сохранённом запросе значениями,
// запрошенными у пользователя. Числовое значение вставляется как есть; всё
// прочее — как экранированный строковый SQL-литерал в кавычках (значения не
// могут внедрить SQL). :name внутри строкового литерала и оператор ::cast
// игнорируются. Возвращает SQL без изменений, если параметров нет.
func (r *REPL) bindParams(sql string) (string, error) {
	names := queryParams(sql)
	if len(names) == 0 {
		return sql, nil
	}
	values := map[string]string{}
	for _, name := range names {
		v := strings.TrimSpace(r.readLine(fmt.Sprintf("  :%s = ", name)))
		values[name] = paramLiteral(v)
	}
	return applyParams(sql, values), nil
}

type paramOcc struct {
	start, end int
	name       string
}

// paramOccs находит вхождения :name параметров вне строковых литералов/комментариев
// и ::cast, используя маску с сохранением позиций.
func paramOccs(sql string) []paramOcc {
	masked := sqlsplit.Mask(sql)
	var occs []paramOcc
	for _, m := range paramRe.FindAllStringIndex(masked, -1) {
		if m[0] > 0 && masked[m[0]-1] == ':' {
			continue // часть ::cast
		}
		occs = append(occs, paramOcc{m[0], m[1], masked[m[0]+1 : m[1]]})
	}
	return occs
}

// queryParams возвращает уникальные :name параметры в порядке первого появления.
func queryParams(sql string) []string {
	var names []string
	seen := map[string]bool{}
	for _, o := range paramOccs(sql) {
		if !seen[o.name] {
			seen[o.name] = true
			names = append(names, o.name)
		}
	}
	return names
}

// applyParams заменяет каждое вхождение :name на values[name] (уже закодированное
// как литерал).
func applyParams(sql string, values map[string]string) string {
	var b strings.Builder
	prev := 0
	for _, o := range paramOccs(sql) {
		b.WriteString(sql[prev:o.start])
		b.WriteString(values[o.name])
		prev = o.end
	}
	b.WriteString(sql[prev:])
	return b.String()
}

func paramLiteral(v string) string {
	if numLitRe.MatchString(v) {
		return v
	}
	return sqlLiteral(v)
}

// doQueries выводит список сохранённых запросов.
func (r *REPL) doQueries() {
	names := r.queries.Names()
	if len(names) == 0 {
		fmt.Fprintln(r.out, "no saved queries (\\save <name> [sql])")
		return
	}
	for _, n := range names {
		sql, _ := r.queries.Get(n)
		fmt.Fprintf(r.out, "  %-16s %s\n", n, oneLine(sql))
	}
}

// doUnsave удаляет сохранённый запрос.
func (r *REPL) doUnsave(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: \\unsave <name>")
	}
	if err := r.queries.Delete(args[0]); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "removed %q\n", args[0])
	return nil
}

// watchableMeta — диагностические meta-команды, которые \watch может безопасно
// переисполнять на каждом тике (read-only, не меняют контекст/не пишут).
var watchableMeta = map[string]bool{
	"\\activity": true, "\\blockers": true, "\\locks": true, "\\longtx": true,
	"\\statements": true, "\\workload": true,
}

const watchableMetaList = "\\activity, \\blockers, \\locks, \\longtx, \\statements"

// doWatch периодически перезапускает запрос на чтение (или диагностическую
// meta-команду) до Ctrl-C, очищая экран на каждом тике (как watch / psql \watch).
func (r *REPL) doWatch(args []string, raw string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: \\watch [interval] <query>")
	}
	interval := 2 * time.Second
	skip := 1 // "\watch"
	// Ведущий токен, похожий на интервал, должен парситься в ПОЛОЖИТЕЛЬНУЮ
	// длительность; "0s", "-1s" или "0" отвергаются (иначе \watch уйдёт в
	// busy-loop).
	if looksLikeInterval(args[0]) {
		d, ok := parseInterval(args[0])
		if !ok {
			return fmt.Errorf("invalid interval %q (must be positive, e.g. 500ms, 5s, 2m, or bare seconds)", args[0])
		}
		interval = d
		skip = 2 // "\watch <interval>"
	}
	// Берём запрос дословно из исходной строки, чтобы точные пробелы строкового
	// литерала пережили разбиение Fields.
	sql := strings.TrimSpace(rawTail(raw, skip))
	if sql == "" {
		return fmt.Errorf("usage: \\watch [interval] <query | \\diag-command>")
	}
	// \watch может обновлять либо read SQL, либо диагностическую meta-команду
	// (\activity/\blockers/\locks/\longtx/\statements) — Feature 10 live refresh.
	// Прочие meta-команды не зацикливаем (могут менять контекст/писать).
	isMeta := strings.HasPrefix(sql, "\\")
	if isMeta {
		fields := strings.Fields(sql)
		cmd := strings.ToLower(fields[0])
		if !watchableMeta[cmd] {
			return fmt.Errorf("\\watch can refresh read SQL or a diagnostic command: %s", watchableMetaList)
		}
		// \statements snapshot/diff ведут baseline (r.lastWorkload): зацикленные,
		// они затирали бы baseline на каждом тике, делая diff бессмысленным.
		// Под \watch зацикливаем только голый \statements (живой top-N).
		if (cmd == "\\statements" || cmd == "\\workload") && len(fields) > 1 {
			sub := strings.ToLower(fields[1])
			if sub == "snapshot" || sub == "diff" {
				return fmt.Errorf("\\watch cannot loop %s %s (it would clobber the workload baseline each tick); watch the bare %s instead", cmd, sub, cmd)
			}
		}
	} else {
		if execution.IsWrite(sql) {
			return fmt.Errorf("\\watch is read-only")
		}
		sql = strings.TrimSuffix(sql, ";")
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	// По Ctrl-C отменяем сам выполняющийся запрос (а не только сон между тиками),
	// чтобы медленный тик прерывался сразу, как обещает баннер.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-sig:
			cancel()
		case <-ctx.Done():
		}
	}()

	for {
		fmt.Fprint(r.out, "\x1b[H\x1b[2J") // очистить экран, курсор в начало
		fmt.Fprintf(r.out, "%s  every %s  (%s) — Ctrl-C to stop\n\n", r.now(), interval, r.contextLabel())
		if isMeta {
			_, _ = r.runMeta(sql)
		} else {
			r.execReadCtx(ctx, sql)
		}
		if ctx.Err() != nil {
			fmt.Fprintln(r.out, "\nstopped")
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(r.out, "\nstopped")
			return nil
		case <-time.After(interval):
		}
	}
}

// parseInterval принимает Go-длительность ("5s", "2m") или голое целое (секунды).
// Длительность должна быть положительной ("0s"/"-1s" отвергаются).
func parseInterval(s string) (time.Duration, bool) {
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return 0, false
		}
		return d, true
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return time.Duration(n) * time.Second, true
	}
	return 0, false
}

// looksLikeInterval сообщает, является ли s числом с необязательной единицей
// времени (чтобы \watch отличал токен интервала от начала запроса).
var intervalRe = regexp.MustCompile(`(?i)^-?[0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h)?$`)

func looksLikeInterval(s string) bool { return intervalRe.MatchString(s) }
