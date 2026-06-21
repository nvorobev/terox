package repl

import (
	"fmt"
	"os"
	"strings"

	"terox/internal/cluster"
	"terox/internal/complete"
)

// metaCommands — мета-команды для дополнения строк, начинающихся с обратной
// косой черты; единый источник для tab-дополнения, включая все алиасы.
var metaCommands = []string{
	"\\use", "\\c", "\\connect", "\\shard", "\\s", "\\shards", "\\l", "\\list", "\\add",
	"\\write", "\\write_approve", "\\timeout", "\\maxrows", "\\timing", "\\x", "\\impact", "\\suggest",
	"\\e", "\\edit", "\\migrate", "\\m", "\\i", "\\include", "\\dt", "\\dn", "\\di", "\\d", "\\watch", "\\g", "\\gx", "\\grep",
	"\\count", "\\locate", "\\find", "\\diff", "\\ping", "\\explain", "\\doctor", "\\heal", "\\compare",
	"\\export", "\\save", "\\run", "\\queries", "\\unsave", "\\completion",
	"\\activity", "\\blockers", "\\locks", "\\longtx",
	"\\statements", "\\workload", "\\cancel", "\\terminate", "\\copy", "\\advise", "\\lint", "\\sizes",
	"\\editor", "\\layout", "\\h", "\\history", "\\help", "\\?", "\\q", "\\quit",
}

// completer реализует readline.AutoCompleter с учётом каталога: мета-команды
// после обратной косой, отношения после FROM/JOIN/UPDATE/INTO, иначе — ключевые
// слова SQL и все объекты/функции каталога.
type completer struct{ r *REPL }

func newCompleter(r *REPL) *completer { return &completer{r: r} }

// isWordByte сообщает, входит ли байт в слово. '$' допустим внутри
// некавыченного идентификатора PostgreSQL (например, invoice$item).
func isWordByte(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// currentWord возвращает набираемый токен: завершающую цепочку символов
// идентификатора и точек (так "table.col" остаётся одним словом), с ведущей
// обратной косой для мета-команд. Останавливается на "(", ",", пробелах и
// операторах: в "extract(ep" слово — "ep".
func currentWord(head string) string {
	i := len(head)
	for i > 0 && (isWordByte(head[i-1]) || head[i-1] == '.') {
		i--
	}
	if i > 0 && head[i-1] == '\\' && (i == 1 || head[i-2] == ' ' || head[i-2] == '\t') {
		i--
	}
	return head[i:]
}

// suggestions возвращает суффиксы дополнения для head и число рун уже
// набранного слова, которое они заменяют. Мета-команды обрабатываются по
// таблицам аргументов в памяти, SQL — типизированным контекстным движком.
func (c *completer) suggestions(line string, pos int) (subs []string, replaceRunes int) {
	if pos > len(line) {
		pos = len(line)
	}
	head := line[:pos]
	if trimmed := strings.TrimLeft(head, " \t"); strings.HasPrefix(trimmed, "\\") {
		// SQL/предикатный хвост мета-команды (\watch, \save, \count/\locate WHERE,
		// \explain) дополняется настоящим SQL-движком, чтобы там тоже появлялись
		// колонки/таблицы/ключевые слова.
		if sqlText, ok := c.metaSQLTail(head); ok {
			return c.sqlSuffixes(sqlText, len(sqlText))
		}
		word := currentWord(head)
		var sources [][]string
		if !strings.ContainsAny(trimmed, " \t") {
			sources = [][]string{metaCommands} // ещё набирается имя команды
		} else {
			sources = c.metaArgs(head)
		}
		return suffixes(word, sources), len([]rune(word))
	}

	return c.sqlSuffixes(line, pos)
}

// filterCandidates применяет ЕДИНОЕ правило отбора кандидатов автодополнения, на
// котором держится инвариант «ghost = выделенная строка меню = список Tab»: Insert
// должен начинаться с prefix без учёта регистра; защита от среза за границей при
// сведении регистра (İ, K); суффикс — остаток Insert после prefix; повторяющиеся
// суффиксы отбрасываются. fn вызывается для каждого подходящего кандидата с его
// суффиксом; пустой suf (точное совпадение набранного) передаётся без дедупа, а
// политику по нему (exact-флаг/ранний выход) задаёт вызывающий. Возврат false из fn
// прекращает обход. Держать правило в одном месте обязательно — иначе три
// потребителя (меню, список Tab, ghost) разойдутся.
func filterCandidates(cands []complete.Candidate, prefix string, fn func(cand complete.Candidate, suf string) bool) {
	lp := strings.ToLower(prefix)
	seen := map[string]bool{}
	for _, cand := range cands {
		if !strings.HasPrefix(strings.ToLower(cand.Insert), lp) || len(cand.Insert) < len(prefix) {
			continue
		}
		suf := cand.Insert[len(prefix):]
		if suf != "" {
			if seen[suf] {
				continue
			}
			seen[suf] = true
		}
		if !fn(cand, suf) {
			return
		}
	}
}

// sqlSuffixes запускает SQL-движок дополнения для текста до pos и возвращает
// суффиксы кандидатов и число рун набранного слова, которое они заменяют.
// Используется и обычными SQL-строками, и SQL-хвостами мета-команд.
func (c *completer) sqlSuffixes(line string, pos int) (subs []string, replaceRunes int) {
	res := c.sqlResult(line, pos, true) // явный Tab: нужные колонки грузим синхронно
	if res.ReplaceStart > len(line) {
		return nil, 0
	}
	prefix := line[res.ReplaceStart:]
	filterCandidates(res.Candidates, prefix, func(_ complete.Candidate, suf string) bool {
		if suf != "" {
			subs = append(subs, suf)
		}
		return true
	})
	return subs, len([]rune(prefix))
}

// metaSQLTail возвращает SQL-текст, конец которого — курсор, для мета-команды с
// SQL/предикатным хвостом, и ok=true, когда курсор в этом хвосте (а не на
// фиксированных ведущих аргументах). Для \count/\locate таблица вплетается в
// синтетический SELECT, чтобы её колонки разрешались в предикате.
func (c *completer) metaSQLTail(head string) (string, bool) {
	trimmed := strings.TrimLeft(head, " \t")
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", false
	}
	cmd := strings.ToLower(fields[0])
	word := currentWord(head)
	args := fields[1:]
	if word != "" && len(args) > 0 {
		args = args[:len(args)-1] // полностью введённые аргументы (без частичного слова)
	}
	argIdx := len(args)

	switch cmd {
	case "\\watch":
		skip := 1
		if len(fields) >= 2 && looksLikeInterval(fields[1]) {
			skip = 2
		}
		if argIdx < skip-1 {
			return "", false
		}
		return rawTail(head, skip), true
	case "\\save":
		if argIdx < 1 { // ещё набирается <name>
			return "", false
		}
		return rawTail(head, 2), true
	case "\\explain":
		if argIdx >= 1 {
			switch strings.ToLower(args[0]) {
			case "-f", "--file", "diff", "save", "compare":
				return "", false // путь / подкоманда, обрабатывается в metaArgs
			}
		}
		// Пропускаем ведущие флаги EXPLAIN, чтобы найти начало запроса.
		skip := 1
		for i := 0; i < len(args); {
			switch strings.ToLower(args[i]) {
			case "analyze", "analyse", "--first", "-1", "--all", "--outliers":
				skip++
				i++
			case "--shard", "-s", "--sample":
				skip += 2
				i += 2
			default:
				i = len(args)
			}
		}
		return rawTail(head, skip), true
	case "\\count", "\\locate", "\\find":
		if argIdx < 1 { // ещё на токене таблицы → дополнение имени таблицы
			return "", false
		}
		table := args[0]
		if _, err := splitQualified(table); err != nil {
			return "", false
		}
		pred := stripLeadingWhere(strings.TrimSpace(rawTail(head, 2)))
		return "select * from " + table + " where " + pred, true
	}
	return "", false
}

// sqlResult запускает движок дополнения для SQL-головы, лениво подгружая колонки
// нужных отношений. При sync=true (явный Tab) недостающие колонки берутся
// синхронно, иначе (ghost на каждое нажатие) — в фоне и появляются при
// последующей отрисовке. Все чтения/записи каталога сериализуются под catalogMu,
// чтобы фоновые загрузчики не гонялись с движком.
func (c *completer) sqlResult(line string, pos int, sync bool) complete.Result {
	r := c.r
	if pos > len(line) {
		pos = len(line)
	}
	cat := r.completeCatalog()
	if cat == nil {
		return complete.Result{ReplaceStart: pos}
	}
	targets := append([]cluster.Shard(nil), r.targets...)
	conc := r.cfg.ProbeConcurrency(len(targets))

	r.catalogMu.Lock()
	refs := complete.NeededColumnsAt(line, pos, cat)
	var missing []complete.RelRef
	for _, ref := range refs {
		if !cat.ColumnsLoaded(ref.Schema, ref.Name) {
			missing = append(missing, ref)
		}
	}
	r.catalogMu.Unlock()

	for _, ref := range missing {
		if sync {
			r.loadColumns(cat, targets, conc, ref)
		} else {
			r.loadColumnsAsync(cat, targets, conc, ref)
		}
	}

	r.catalogMu.Lock()
	cat.IncludeSystem = r.showSystemCatalog
	res := complete.CompleteAt(line, pos, cat)
	r.catalogMu.Unlock()
	return res
}

// ghostHint возвращает встроенный ghost-суффикс и только-для-показа аннотацию
// верхнего кандидата (его сигнатуру/тип, бейдж покрытия по шардам и хвост "+N"
// при наличии других совпадений). Аннотация никогда не вставляется — отрисовщик
// показывает её тускло и стирает, — так что сигнатуры и покрытие видны в живом UI.
// autoTrigger сообщает, должно ли срабатывать авто-дополнение (ghost + живое
// меню) для head. Срабатывает только при наборе токена: последний символ —
// символ слова или квалификатор ".". Ничего не происходит на пустой строке или
// сразу после пробела, запятой, ";", ")" или оператора. Мета-команды (\...)
// срабатывают и при наборе команды/аргумента.
func autoTrigger(head string) bool {
	if head == "" {
		return false
	}
	last := head[len(head)-1]
	if strings.HasPrefix(strings.TrimLeft(head, " \t"), "\\") {
		return last != ' ' && last != '\t'
	}
	return last == '.' || last == '_' || last == '$' ||
		(last >= 'a' && last <= 'z') || (last >= 'A' && last <= 'Z') ||
		(last >= '0' && last <= '9')
}

func (c *completer) ghostHint(head string) (ghost, annotation string) {
	// Авто-подсказка только при наборе токена (см. autoTrigger): ничего на пустой
	// строке, после пробела/запятой/";" или другого разделителя. Квалификатор "."
	// всё же даёт подсказку (углубление в схему).
	if !autoTrigger(head) {
		return "", ""
	}
	trimmed := strings.TrimLeft(head, " \t")
	if strings.HasPrefix(trimmed, "\\") {
		subs, _ := c.suggestions(head, len(head))
		return ghostOf(subs), ""
	}
	res := c.sqlResult(head, len(head), false) // на каждое нажатие: колонки только в фоне
	prefix := head[res.ReplaceStart:]
	var subs []string
	var best complete.Candidate
	have, exact := false, false
	filterCandidates(res.Candidates, prefix, func(cand complete.Candidate, suf string) bool {
		if suf == "" {
			// Набранное — уже полное корректное имя. Не подсказываем более длинную
			// альтернативу (набрав "shop_items_cnt", не толкаем к
			// "shop_items_cnt_aggregate"); полный список остаётся в меню.
			exact = true
			return false
		}
		if !have {
			best, have = cand, true
		}
		subs = append(subs, suf)
		return true
	})
	if exact || !have {
		return "", ""
	}
	return ghostOf(subs), annotate(best, len(subs))
}

func ghostOf(subs []string) string {
	switch len(subs) {
	case 0:
		return ""
	case 1:
		return subs[0]
	default:
		// Предпочитаем однозначный общий префикс (ghost дописывает только общее).
		// Когда кандидаты расходятся сразу — например, после "schema." у таблиц и
		// функций нет общего префикса — берём верхнего по рангу кандидата, чтобы
		// подсказка всё же появлялась при наборе (аннотация "+N" указывает, что
		// есть и другие).
		if lcp := longestCommonPrefix(subs); lcp != "" {
			return lcp
		}
		return subs[0]
	}
}

// annotate отрисовывает тусклую подсказку кандидата: сигнатуру функции, тип
// колонки (":type"), бейдж покрытия и хвост "+N" для дополнительных совпадений.
func annotate(cand complete.Candidate, n int) string {
	var parts []string
	if cand.Detail != "" {
		switch cand.Kind {
		case complete.KColumn:
			parts = append(parts, ":"+cand.Detail)
		default:
			parts = append(parts, cand.Detail)
		}
	}
	if cand.Coverage != "" {
		parts = append(parts, cand.Coverage)
	}
	s := strings.Join(parts, " ")
	if s != "" {
		s = "  " + s
	}
	if n > 1 {
		s += fmt.Sprintf("  +%d", n-1)
	}
	return s
}

// metaArgs возвращает кандидатов дополнения для аргумента мета-команды в
// зависимости от команды и того, какой аргумент набирается.
func (c *completer) metaArgs(head string) [][]string {
	fields := strings.Fields(head)
	if len(fields) == 0 {
		return nil
	}
	cmd := strings.ToLower(fields[0])
	word := currentWord(head)
	// Считаем полностью введённые аргументы (без набираемого частичного слова).
	args := fields[1:]
	if word != "" && len(args) > 0 {
		args = args[:len(args)-1]
	}
	argIdx := len(args)

	switch cmd {
	case "\\use":
		return [][]string{c.r.cfg.ServiceNames()}
	case "\\c":
		if argIdx == 0 {
			return [][]string{c.r.cfg.StorageNames(c.r.service)}
		}
		return [][]string{shardSelectorTokens(c.r.shards)}
	case "\\shard", "\\s", "\\g", "\\gx":
		return [][]string{shardSelectorTokens(c.r.shards)}
	case "\\run", "\\unsave":
		if c.r.queries != nil {
			return [][]string{c.r.queries.Names()}
		}
	case "\\write", "\\write_approve", "\\impact", "\\suggest", "\\timing":
		return [][]string{{"on", "off"}}
	case "\\count", "\\locate", "\\find":
		// Первый аргумент — [schema.]table; после второй точки ("schema.table.")
		// углубляемся в колонки первичного ключа таблицы для сокращения поиска по
		// ключу "table.pkey=value".
		if argIdx == 0 {
			if pk := c.pkeyArgCandidates(word); pk != nil {
				return [][]string{pk}
			}
			return [][]string{complete.RelationArgCandidates(c.r.completeCatalog())}
		}
	case "\\diff", "\\d":
		// Первый аргумент — [schema.]table — дополняем из каталога.
		if argIdx == 0 {
			return [][]string{complete.RelationArgCandidates(c.r.completeCatalog())}
		}
	case "\\completion":
		return [][]string{{"status", "reload", "system"}}
	case "\\doctor":
		return [][]string{{"--all"}}
	case "\\heal":
		return [][]string{{"--apply"}}
	case "\\help", "\\?":
		names := make([]string, len(helpEntries))
		for i, e := range helpEntries {
			names[i] = e.names[0]
		}
		return [][]string{names}
	case "\\timeout":
		return [][]string{{"500ms", "1s", "5s", "30s", "60s", "5min", "off"}}
	case "\\compare":
		// service/storage — чисто дополняется только первый сегмент ('/' разбивает
		// токен); при наличии слэша подавляем, чтобы избежать двойной вставки.
		if rawArg := lastArg(head); !strings.Contains(rawArg, "/") {
			return [][]string{c.r.compareTargets()}
		}
	case "\\migrate", "\\m", "\\i":
		return [][]string{pathCandidates(lastArg(head))}
	case "\\export":
		if argIdx == 0 {
			return [][]string{{"csv", "json"}}
		}
		return [][]string{pathCandidates(lastArg(head))}
	case "\\explain":
		// Путь нужен только \explain -f <path>; иначе это SQL-запрос, обрабатываемый
		// обычным дополнением (поэтому здесь не считаем его мета-аргументом).
		if len(args) > 0 && args[len(args)-1] == "-f" {
			return [][]string{pathCandidates(lastArg(head))}
		}
	}
	return nil
}

// pkeyArgCandidates возвращает дополнения "<tableprefix>.<pkcol>", когда `word`
// имеет вид "[schema.]table.<colpartial>", так что колонки первичного ключа
// выпадают после второй точки (сокращение поиска по ключу \find/\count).
// Возвращает nil, когда `word` ещё на позиции таблицы (тогда вызывающий
// предлагает имена таблиц).
func (c *completer) pkeyArgCandidates(word string) []string {
	lastDot := strings.LastIndex(word, ".")
	if lastDot < 0 || word[:lastDot] == "" {
		return nil
	}
	parts, err := splitQualified(word[:lastDot])
	if err != nil {
		return nil
	}
	cat := c.r.completeCatalog()
	if cat == nil {
		return nil
	}
	var schema, table string
	switch len(parts) {
	case 1:
		// Голое "name." — схема углубляется в свои таблицы (обрабатывается в другом
		// месте); только отношение углубляется в колонки своего ключа.
		if cat.HasSchema(parts[0]) || !cat.HasRelation("", parts[0]) {
			return nil
		}
		table = parts[0]
	case 2:
		schema, table = parts[0], parts[1]
		if !cat.HasRelation(schema, table) {
			return nil
		}
	default:
		return nil
	}
	pk, _ := c.r.pkColumns(schema, table)
	prefix := word[:lastDot+1]
	out := make([]string, 0, len(pk)) // не-nil: мы на позиции колонки
	for _, col := range pk {
		out = append(out, prefix+complete.QuoteIdent(col, cat.Reserved))
	}
	return out
}

// lastArg возвращает сырой токен аргумента у курсора (текст после последнего
// пробела), сохраняя '/' и другие не-словные байты для путей/квалифицированных
// аргументов.
func lastArg(head string) string {
	if i := strings.LastIndexAny(head, " \t"); i >= 0 {
		return head[i+1:]
	}
	return head
}

// shardSelectorTokens — метки шардов плюс "all" для селекторов \shard / \c.
func shardSelectorTokens(shards []cluster.Shard) []string {
	out := make([]string, 0, len(shards)+1)
	out = append(out, "all")
	for _, s := range shards {
		out = append(out, s.Label)
	}
	return out
}

// pathCandidates перечисляет записи каталога, подходящие под частичный путь
// (каталоги получают завершающий "/"), для мета-команд с файловым аргументом.
func pathCandidates(rawArg string) []string {
	dir, base := ".", rawArg
	if i := strings.LastIndex(rawArg, "/"); i >= 0 {
		dir, base = rawArg[:i+1], rawArg[i+1:]
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base) {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		out = append(out, name)
	}
	return out
}

// compareTargets перечисляет комбинации "service/storage" для дополнения \compare.
func (r *REPL) compareTargets() []string {
	var out []string
	for _, svc := range r.cfg.ServiceNames() {
		for _, st := range r.cfg.StorageNames(svc) {
			out = append(out, svc+"/"+st)
		}
	}
	return out
}

// suffixes возвращает суффиксы дополнения (кандидат минус набранное слово).
func suffixes(word string, sources [][]string) []string {
	lw := strings.ToLower(word)
	wlen := len(word)
	var out []string
	seen := map[string]bool{}
	for _, src := range sources {
		for _, cand := range src {
			if len(cand) < wlen || seen[cand] {
				continue
			}
			if strings.HasPrefix(strings.ToLower(cand), lw) {
				seen[cand] = true
				out = append(out, cand[wlen:])
			}
		}
	}
	return out
}

// Do реализует readline.AutoCompleter.
func (c *completer) Do(line []rune, pos int) ([][]rune, int) {
	// readline даёт pos как индекс РУНЫ, но suggestions/sqlResult и движок
	// дополнения режут строку по БАЙТОВОМУ смещению (line[:pos]). Конвертируем,
	// чтобы многобайтовый префикс (например, кириллический строковый литерал) не
	// портил дополняемое слово. Возвращаемая длина замены остаётся в рунах, как
	// ждёт readline.
	if pos > len(line) {
		pos = len(line)
	}
	bytePos := len(string(line[:pos]))
	subs, n := c.suggestions(string(line), bytePos)
	out := make([][]rune, 0, len(subs))
	for _, s := range subs {
		out = append(out, []rune(s))
	}
	return out, n
}

func longestCommonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	return p
}
