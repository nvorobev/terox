package complete

import "strings"

// significant возвращает токены head без пробелов и комментариев.
func significant(toks []Token) []Token {
	out := make([]Token, 0, len(toks))
	for _, t := range toks {
		if t.Kind == TWhitespace || t.Kind == TComment {
			continue
		}
		out = append(out, t)
	}
	return out
}

// lowerWord возвращает слово для сопоставления с ключевыми словами: обычное слово —
// в нижнем регистре; идентификатор в кавычках сохраняет регистр (чтобы не сливаться
// с ключевым словом); "" для не-слов.
func lowerWord(t Token) string {
	switch t.Kind {
	case TWord:
		return strings.ToLower(t.Text)
	case TQIdent:
		// Регистр сохраняем (кавыченный идентификатор не должен сливаться с ключевым
		// словом), но кавычки снимаем корректно: одну крайнюю с каждой стороны и
		// "" → " — как identName, иначе "a""b" вернулось бы как a""b.
		return identName(t)
	}
	return ""
}

// identName возвращает голое имя идентификатора (без кавычек).
func identName(t Token) string {
	switch t.Kind {
	case TWord:
		return t.Text
	case TQIdent:
		s := strings.TrimPrefix(t.Text, `"`)
		s = strings.TrimSuffix(s, `"`)
		return strings.ReplaceAll(s, `""`, `"`)
	case TIncomplete:
		return strings.TrimPrefix(t.Text, `"`)
	}
	return t.Text
}

// analyzePrefix находит начало замены, набираемый частичный токен и цепочку
// квалификаторов через точку перед ним (не включая частичный).
func analyzePrefix(toks []Token, head string) (replaceStart int, partial string, quals []string) {
	replaceStart = len(head)
	// Индекс последнего токена, если он кончается у курсора и похож на идентификатор.
	i := len(toks) - 1
	if i >= 0 && toks[i].End == len(head) {
		t := toks[i]
		if t.Kind == TWord || t.Kind == TQIdent ||
			(t.Kind == TIncomplete && strings.HasPrefix(t.Text, `"`)) {
			partial = t.Text
			replaceStart = t.Start
			i--
		}
	}
	// Идём назад по парам ". ident" (пропуская пробелы/комментарии), собирая
	// цепочку квалификаторов.
	prev := func(k int) int {
		for k >= 0 && (toks[k].Kind == TWhitespace || toks[k].Kind == TComment) {
			k--
		}
		return k
	}
	i = prev(i)
	for i >= 1 && toks[i].Kind == TPunct && toks[i].Text == "." {
		j := prev(i - 1)
		if j < 0 || (toks[j].Kind != TWord && toks[j].Kind != TQIdent) {
			break
		}
		quals = append([]string{identName(toks[j])}, quals...)
		i = prev(j - 1)
	}
	return replaceStart, partial, quals
}

// scopeRel — отношение, попавшее в область видимости через FROM/JOIN/UPDATE/USING/INTO.
type scopeRel struct {
	schema, name, alias string
	isCTE               bool
	// isDerived помечает производную таблицу (подзапрос в FROM/JOIN: `(SELECT …) sub`,
	// в т.ч. LATERAL). У неё нет записи в каталоге, поэтому имена выводимых колонок
	// разобраны из списка SELECT подзапроса и лежат в derivedCols (для `sub.`).
	isDerived   bool
	derivedCols []string
}

// Ключевые слова, завершающие область ссылки на таблицу / списка столбцов.
var clauseStop = map[string]bool{
	"where": true, "group": true, "order": true, "having": true, "limit": true,
	"offset": true, "returning": true, "set": true, "values": true, "on": true,
	"using": true, "union": true, "intersect": true, "except": true, "window": true,
	"fetch": true, "for": true, "into": true, ";": true,
}

var joinWords = map[string]bool{
	"join": true, "left": true, "right": true, "inner": true, "outer": true,
	"cross": true, "full": true, "natural": true, "lateral": true,
}

// clauseContinuationKeywords следуют за полной ссылкой на таблицу в SELECT.
var clauseContinuationKeywords = []string{
	"where", "group by", "order by", "having", "limit", "offset",
	"join", "left join", "right join", "inner join", "cross join", "full join",
	"on", "using", "union", "union all", "intersect", "except", "as", "window",
	"fetch", "for update", "for share",
}

// selectListContinuation — ключевые слова, которые могут следовать за полным
// элементом списка SELECT (прежде всего FROM). Предлагаются с приоритетом, чтобы
// "f" после столбца дополнялось до FROM, а не до функции (floor/format/...).
var selectListContinuation = []string{
	"from", "as", "into", "union", "union all", "intersect", "except",
}

// afterCompleteSelectItem сообщает, стоит ли курсор в списке SELECT сразу после
// полного элемента (идентификатор, литерал, ")" или "*"), где естественно
// следует ключевое слово вроде FROM, в отличие от позиции сразу после SELECT,
// запятой или оператора, где ожидается новое выражение.
func afterCompleteSelectItem(sig []Token) bool {
	if len(sig) == 0 {
		return false
	}
	t := sig[len(sig)-1]
	switch t.Kind {
	case TWord:
		switch strings.ToLower(t.Text) {
		case "select", "distinct", "all", "as", "and", "or", "not",
			"case", "when", "then", "else", "in", "like", "between":
			return false
		}
		return true
	case TNumber, TString:
		return true
	case TPunct:
		return t.Text == ")"
	case TOp:
		return t.Text == "*"
	}
	return false
}

// inImmediateTableSlot сообщает, стоит ли курсор там, где ожидается имя таблицы
// (сразу после FROM/JOIN/USING/слова join/запятой), в отличие от позиции после
// ссылки на таблицу, где ожидаются ключевые слова продолжения предложения.
func inImmediateTableSlot(sig []Token) bool {
	if len(sig) == 0 {
		return true
	}
	t := sig[len(sig)-1]
	if t.Kind == TPunct && t.Text == "," {
		return true
	}
	if t.Kind == TWord {
		w := strings.ToLower(t.Text)
		if w == "from" || w == "join" || w == "using" || joinWords[w] {
			return true
		}
	}
	return false
}

// gatherScope извлекает отношения/псевдонимы в области видимости и имена CTE из токенов sig.
func gatherScope(sig []Token) []scopeRel {
	low := make([]string, len(sig))
	for i, t := range sig {
		low[i] = lowerWord(t)
	}
	var out []scopeRel

	// Имена WITH cte [ (cols) ] AS [ NOT ] [ MATERIALIZED ] ( ... ), возможно
	// несколько. Учитываем глубину скобок, чтобы SELECT в теле одного CTE не
	// прерывал разбор, и распознаём необязательный список столбцов
	// `cte(col, ...)` между именем и AS.
	if len(low) > 0 && low[0] == "with" {
		i := 1
		if i < len(low) && low[i] == "recursive" {
			i++
		}
		depth := 0
		for i < len(sig) {
			if sig[i].Kind == TPunct && sig[i].Text == "(" {
				depth++
				i++
				continue
			}
			if sig[i].Kind == TPunct && sig[i].Text == ")" {
				if depth > 0 {
					depth--
				}
				i++
				continue
			}
			if depth == 0 {
				// Ключевое слово верхнего уровня завершает область заголовка CTE.
				if low[i] == "select" || low[i] == "insert" || low[i] == "update" ||
					low[i] == "delete" || low[i] == "merge" {
					break
				}
				if sig[i].Kind == TWord || sig[i].Kind == TQIdent {
					name := identName(sig[i])
					// Пропускаем необязательный список столбцов в скобках перед AS.
					j := i + 1
					if j < len(sig) && sig[j].Kind == TPunct && sig[j].Text == "(" {
						d := 1
						j++
						for j < len(sig) && d > 0 {
							if sig[j].Kind == TPunct && sig[j].Text == "(" {
								d++
							} else if sig[j].Kind == TPunct && sig[j].Text == ")" {
								d--
							}
							j++
						}
					}
					if j < len(sig) && lowerWord(sig[j]) == "as" {
						out = append(out, scopeRel{name: name, alias: name, isCTE: true})
					}
				}
			}
			i++
		}
	}

	readRef := func(i int) (scopeRel, int) {
		// Формат ссылки: [схема .] имя [ [AS] псевдоним ]
		var sr scopeRel
		if sig[i].Kind != TWord && sig[i].Kind != TQIdent {
			return sr, i + 1
		}
		sr.name = identName(sig[i])
		i++
		if i+1 < len(sig) && sig[i].Kind == TPunct && sig[i].Text == "." &&
			(sig[i+1].Kind == TWord || sig[i+1].Kind == TQIdent) {
			sr.schema = sr.name
			sr.name = identName(sig[i+1])
			i += 2
		}
		// табличная функция/подзапрос в скобках — определение псевдонима здесь пропускаем
		if i < len(sig) && low[i] == "as" {
			i++
			if i < len(sig) && (sig[i].Kind == TWord || sig[i].Kind == TQIdent) {
				sr.alias = identName(sig[i])
				i++
			}
		} else if i < len(sig) && (sig[i].Kind == TWord || sig[i].Kind == TQIdent) &&
			!clauseStop[low[i]] && !joinWords[low[i]] && low[i] != "on" {
			sr.alias = identName(sig[i])
			i++
		}
		if sr.alias == "" {
			sr.alias = sr.name
		}
		return sr, i
	}

	for i := 0; i < len(sig); i++ {
		w := low[i]
		inRefPos := false
		switch {
		case w == "from" || w == "join" || w == "using" || w == "update":
			inRefPos = true
		case w == "into":
			// цель INSERT INTO / MERGE INTO
			inRefPos = true
		}
		if !inRefPos {
			continue
		}
		j := i + 1
		// FROM может начинать список ссылок через запятую.
		for j < len(sig) {
			// LATERAL перед подзапросом/функцией — пропускаем само ключевое слово.
			if low[j] == "lateral" {
				j++
				continue
			}
			if sig[j].Kind == TPunct && sig[j].Text == "(" {
				// Производная таблица: (SELECT …) [AS] alias [(colaliases)]. Разбираем
				// псевдоним и выводимые колонки подзапроса, чтобы `alias.` дополнялся.
				if sr, nj, ok := readDerived(sig, low, j); ok {
					out = append(out, sr)
					j = nj
				} else {
					j = skipParenGroup(sig, j) // не распознали псевдоним — не зацикливаемся
				}
				if j < len(sig) && sig[j].Kind == TPunct && sig[j].Text == "," {
					j++
					continue
				}
				break
			}
			if low[j] != "" && (clauseStop[low[j]] || joinWords[low[j]]) {
				break
			}
			var sr scopeRel
			sr, j = readRef(j)
			if sr.name != "" {
				out = append(out, sr)
			}
			if j < len(sig) && sig[j].Kind == TPunct && sig[j].Text == "," {
				j++
				continue
			}
			break
		}
	}
	return out
}

// matchParen возвращает индекс ")" , парной к "(" в позиции open, либо -1.
func matchParen(sig []Token, open int) int {
	depth := 0
	for i := open; i < len(sig); i++ {
		if sig[i].Kind == TPunct && sig[i].Text == "(" {
			depth++
		} else if sig[i].Kind == TPunct && sig[i].Text == ")" {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// skipParenGroup возвращает индекс сразу за группой "(...)", начинающейся в open;
// если пары нет — конец среза.
func skipParenGroup(sig []Token, open int) int {
	if c := matchParen(sig, open); c >= 0 {
		return c + 1
	}
	return len(sig)
}

// readDerived разбирает производную таблицу `(подзапрос) [AS] alias [(col, …)]`,
// начинающуюся со скобки в позиции open. Возвращает scopeRel с псевдонимом и
// выводимыми колонками подзапроса, индекс за прочитанным и ok. Псевдоним
// обязателен (на безымянную производную таблицу сослаться нельзя).
func readDerived(sig []Token, low []string, open int) (scopeRel, int, bool) {
	closeIdx := matchParen(sig, open)
	if closeIdx < 0 {
		return scopeRel{}, open, false
	}
	inner := sig[open+1 : closeIdx]
	j := closeIdx + 1
	if j < len(sig) && low[j] == "as" {
		j++
	}
	if j >= len(sig) || (sig[j].Kind != TWord && sig[j].Kind != TQIdent) ||
		clauseStop[low[j]] || joinWords[low[j]] || low[j] == "on" {
		return scopeRel{}, j, false
	}
	alias := identName(sig[j])
	j++
	cols := derivedOutputCols(inner)
	// Явный список псевдонимов колонок `alias(c1, c2, …)` перекрывает выводимые имена.
	if j < len(sig) && sig[j].Kind == TPunct && sig[j].Text == "(" {
		if explicit := parenIdentList(sig, j); explicit != nil {
			cols = explicit
		}
		j = skipParenGroup(sig, j)
	}
	return scopeRel{name: alias, alias: alias, isDerived: true, derivedCols: cols}, j, true
}

// parenIdentList читает список идентификаторов внутри группы "(a, b, …)" в позиции
// open (список псевдонимов колонок производной таблицы). Возвращает nil, если в
// группе есть что-то кроме идентификаторов, точек и запятых.
func parenIdentList(sig []Token, open int) []string {
	closeIdx := matchParen(sig, open)
	if closeIdx < 0 {
		return nil
	}
	var out []string
	for i := open + 1; i < closeIdx; i++ {
		switch {
		case sig[i].Kind == TWord || sig[i].Kind == TQIdent:
			out = append(out, identName(sig[i]))
		case sig[i].Kind == TPunct && sig[i].Text == ",":
		default:
			return nil // выражение, не простой список псевдонимов
		}
	}
	return out
}

// stopOutputList — ключевые слова верхнего уровня, завершающие список SELECT
// подзапроса при извлечении имён выводимых колонок.
var stopOutputList = map[string]bool{
	"from": true, "where": true, "group": true, "order": true, "having": true,
	"limit": true, "offset": true, "union": true, "intersect": true, "except": true,
	"window": true, "for": true, "fetch": true,
}

// derivedOutputCols извлекает имена выводимых колонок из списка SELECT подзапроса
// производной таблицы. Консервативно: именуются только элементы с явным `AS имя`
// и простые ссылки на колонку (`c` или `t.c`); выражения, `*` и `t.*` пропускаются
// (их имя без каталога не вывести). Не-SELECT тела (WITH/VALUES/набор-операции)
// дают nil.
func derivedOutputCols(inner []Token) []string {
	low := make([]string, len(inner))
	for i, t := range inner {
		low[i] = lowerWord(t)
	}
	i := 0
	if i >= len(inner) || low[i] != "select" {
		return nil
	}
	i++
	if i < len(inner) && (low[i] == "distinct" || low[i] == "all") {
		i++
		// DISTINCT ON (…) — пропускаем скобочную группу.
		if i < len(inner) && low[i] == "on" && i+1 < len(inner) &&
			inner[i+1].Kind == TPunct && inner[i+1].Text == "(" {
			i = skipParenGroup(inner, i+1)
		}
	}
	var cols []string
	depth := 0
	itemStart := i
	flush := func(end int) {
		if name := outputColName(inner[itemStart:end], low[itemStart:end]); name != "" {
			cols = append(cols, name)
		}
	}
	for ; i < len(inner); i++ {
		switch {
		case inner[i].Kind == TPunct && inner[i].Text == "(":
			depth++
		case inner[i].Kind == TPunct && inner[i].Text == ")":
			if depth > 0 {
				depth--
			}
		case depth == 0 && inner[i].Kind == TPunct && inner[i].Text == ",":
			flush(i)
			itemStart = i + 1
		case depth == 0 && stopOutputList[low[i]]:
			flush(i)
			return cols
		}
	}
	flush(len(inner))
	return cols
}

// reservedOutputWord — слова, которые сами по себе не являются именем колонки
// (литералы/частые функции без имени), чтобы `SELECT true`/`SELECT null` не
// предлагали ложную «колонку».
var reservedOutputWord = map[string]bool{
	"null": true, "true": true, "false": true,
}

// outputColName выводит имя выводимой колонки одного элемента списка SELECT:
// идентификатор после верхнеуровневого AS, либо последний идентификатор простой
// ссылки на колонку (`c` / `schema.t.c`); иначе "".
func outputColName(item []Token, low []string) string {
	if len(item) == 0 {
		return ""
	}
	// Явный AS на верхнем уровне: имя — идентификатор сразу после него.
	depth := 0
	for i := 0; i < len(item); i++ {
		switch {
		case item[i].Kind == TPunct && item[i].Text == "(":
			depth++
		case item[i].Kind == TPunct && item[i].Text == ")":
			if depth > 0 {
				depth--
			}
		case depth == 0 && low[i] == "as":
			if i+1 < len(item) && (item[i+1].Kind == TWord || item[i+1].Kind == TQIdent) {
				return identName(item[i+1])
			}
			return ""
		}
	}
	// Простая ссылка на колонку: только идентификаторы и точки (c / schema.t.c).
	for _, t := range item {
		switch {
		case t.Kind == TWord || t.Kind == TQIdent:
		case t.Kind == TPunct && t.Text == ".":
		default:
			return "" // выражение / функция / звезда — имя без каталога не вывести
		}
	}
	last := item[len(item)-1]
	if last.Kind != TWord && last.Kind != TQIdent {
		return "" // оканчивается точкой (`t.`) — недописанная ссылка
	}
	name := identName(last)
	if reservedOutputWord[strings.ToLower(name)] {
		return ""
	}
	return name
}

// insertColumnTarget распознаёт позицию внутри списка столбцов INSERT —
// `INSERT INTO [схема.]таблица ( <курсор здесь> )` — и возвращает целевое отношение,
// чтобы дополнять его столбцами. Скобка ещё не закрыта (курсор внутри), и до неё нет
// VALUES/SELECT. Это та ветка, которую раньше classify объявлял ("insert-cols"), но
// currentClause никогда не достигал, потому что курсор находится на глубине скобок > 0.
func insertColumnTarget(sig []Token, low []string) (scopeRel, bool) {
	// Индекс верхнеуровневого INTO.
	intoIdx, depth := -1, 0
	for i := range sig {
		switch {
		case sig[i].Kind == TPunct && sig[i].Text == "(":
			depth++
		case sig[i].Kind == TPunct && sig[i].Text == ")":
			if depth > 0 {
				depth--
			}
		}
		if depth == 0 && intoIdx == -1 && low[i] == "into" {
			intoIdx = i
		}
	}
	if intoIdx == -1 {
		return scopeRel{}, false
	}
	// Ссылка на таблицу после INTO: [схема.]таблица.
	j := intoIdx + 1
	if j >= len(sig) || (sig[j].Kind != TWord && sig[j].Kind != TQIdent) {
		return scopeRel{}, false
	}
	var rel scopeRel
	rel.name = identName(sig[j])
	j++
	if j+1 < len(sig) && sig[j].Kind == TPunct && sig[j].Text == "." &&
		(sig[j+1].Kind == TWord || sig[j+1].Kind == TQIdent) {
		rel.schema = rel.name
		rel.name = identName(sig[j+1])
		j += 2
	}
	// Следующий значимый токен — открывающая скобка списка столбцов.
	if j >= len(sig) || sig[j].Kind != TPunct || sig[j].Text != "(" {
		return scopeRel{}, false
	}
	// Скобка должна оставаться открытой до конца (курсор внутри списка).
	d := 0
	for k := j; k < len(sig); k++ {
		if sig[k].Kind == TPunct && sig[k].Text == "(" {
			d++
		} else if sig[k].Kind == TPunct && sig[k].Text == ")" {
			d--
		}
	}
	if d <= 0 {
		return scopeRel{}, false
	}
	rel.alias = rel.name
	return rel, true
}

// expectKind — битовая маска категорий кандидатов для предложения.
type expectKind int

const (
	eRelations expectKind = 1 << iota
	eColumns
	eFunctions
	eKeywords
	eSchemas
	eNewName // новый идентификатор (CREATE ...): из каталога ничего не предлагаем
	eTopLevel
	eCTE // имена CTE в запросе (позиция FROM); также помечает "позицию FROM"
)

// ctxResult — классифицированный контекст автодополнения.
type ctxResult struct {
	expect   expectKind
	scope    []scopeRel
	keywords []string // явный набор ключевых слов (правила по последовательности токенов)
	// schemaDrill помечает позицию с квалификатором "schema.", где результат —
	// объекты этой схемы, упорядоченные по типу, затем по имени.
	schemaDrill bool
}

// topLevelCommands — ключевые слова инструкций, предлагаемые в начале ввода.
var topLevelCommands = []string{
	"select", "insert", "update", "delete", "merge", "with",
	"create", "alter", "drop", "truncate", "comment",
	"grant", "revoke", "explain", "analyze", "vacuum", "reindex", "cluster",
	"copy", "begin", "commit", "rollback", "savepoint", "set", "show", "call",
	"refresh", "table", "values",
}

// pairKeywords реализует правила по последовательности токенов: одно или два
// предыдущих слова задают следующий набор ключевых слов.
func pairKeywords(prev1, prev2 string) []string {
	switch prev1 {
	case "order", "group", "partition":
		return []string{"by"}
	case "nulls":
		return []string{"first", "last"}
	case "is":
		return []string{"null", "not", "true", "false", "unknown", "distinct"}
	case "for":
		return []string{"update", "share", "no key update", "key share", "no", "key"}
	case "union", "intersect", "except":
		return []string{"all", "select"}
	case "insert":
		return []string{"into"}
	case "delete":
		return []string{"from"}
	case "left", "right", "full":
		return []string{"join", "outer join"}
	case "inner", "cross":
		return []string{"join"}
	case "primary", "foreign":
		return []string{"key"}
	case "not":
		if prev2 == "is" {
			return []string{"null", "true", "false", "distinct"}
		}
	}
	return nil
}

// classify определяет, что предложить, по значимым токенам (без частичного
// слова) и цепочке квалификаторов через точку.
func classify(sigAll, scopeSig []Token, partialPresent bool, quals []string, cat *Catalog) ctxResult {
	// sig не включает набираемое частичное слово (вызывающий передаёт полный sig;
	// отбрасываем хвостовой идентификатор, если это частичное слово).
	sig := sigAll
	if partialPresent && len(sig) > 0 {
		last := sig[len(sig)-1]
		if last.Kind == TWord || last.Kind == TQIdent ||
			(last.Kind == TIncomplete && strings.HasPrefix(last.Text, `"`)) {
			sig = sig[:len(sig)-1]
		}
	}
	// И отбрасываем токены квалификаторов (ident . ident .) перед частичным словом.
	for n := 0; n < len(quals); n++ {
		// убираем хвостовые пары "." и идентификатор
		if len(sig) >= 2 && sig[len(sig)-1].Kind == TPunct && sig[len(sig)-1].Text == "." {
			sig = sig[:len(sig)-2]
		} else if len(sig) >= 1 && sig[len(sig)-1].Kind == TPunct && sig[len(sig)-1].Text == "." {
			sig = sig[:len(sig)-1]
		}
	}

	scope := gatherScope(scopeSig)

	// Квалифицировано: schema. или alias./relation.
	if len(quals) > 0 {
		return classifyQualified(quals, scope, cat)
	}

	low := make([]string, len(sig))
	for i, t := range sig {
		low[i] = lowerWord(t)
	}

	// Начало инструкции.
	if len(nonPunct(low)) == 0 {
		return ctxResult{expect: eTopLevel}
	}

	// Правила по последовательности токенов имеют приоритет (только явный набор,
	// без общего перечня ключевых слов).
	p1, p2 := lastTwo(low)
	if ks := pairKeywords(p1, p2); ks != nil {
		return ctxResult{keywords: ks}
	}

	stmt := statementKind(low)
	clause, prevWord := currentClause(sig, low)

	// INSERT INTO t (col, |) — список столбцов целевой таблицы. Курсор внутри
	// скобок (глубина > 0), поэтому currentClause clause-keyword не находит;
	// разрешаем целевую таблицу и предлагаем её столбцы.
	if stmt == "insert" {
		if rel, ok := insertColumnTarget(sig, low); ok {
			return ctxResult{expect: eColumns, scope: []scopeRel{rel}}
		}
	}

	switch clause {
	case "from", "join", "into-target", "update", "truncate", "references":
		// Позиция отношения. После CREATE TABLE — НОВОЕ имя.
		if (clause == "into-target" || clause == "update" || clause == "truncate") && stmt == "create" {
			return ctxResult{expect: eNewName}
		}
		// Всё, на что можно ссылаться из FROM: выбираемые отношения, схемы (можно
		// выбрать схему, ввести '.' и углубиться в её таблицы), имена CTE запроса и
		// табличные (SET-RETURNING) функции вроде generate_series/unnest, но НЕ
		// каталог скалярных функций (eCTE помечает позицию FROM, чтобы генератор
		// оставлял только табличные функции).
		res := ctxResult{expect: eRelations | eCTE | eFunctions | eSchemas, scope: scope}
		// После полной ссылки на таблицу (не в позиции имени таблицы) также
		// предлагаем ключевые слова продолжения, чтобы "... from t w<Tab>" → "where".
		if !inImmediateTableSlot(sig) {
			switch clause {
			case "from", "join":
				res.keywords = clauseContinuationKeywords
			case "update":
				res.keywords = []string{"set"}
			}
		}
		return res
	case "using":
		// DELETE FROM t USING <rel> и MERGE INTO t USING <src> — позиция отношения
		// (gatherScope уже подхватывает это отношение в scope). JOIN ... USING (cols)
		// — список столбцов соединяемых отношений.
		if stmt == "delete" || stmt == "merge" {
			res := ctxResult{expect: eRelations | eCTE | eFunctions | eSchemas, scope: scope}
			if !inImmediateTableSlot(sig) {
				res.keywords = clauseContinuationKeywords
			}
			return res
		}
		return ctxResult{expect: eColumns | eFunctions | eKeywords, scope: scope}
	case "table-kw":
		if stmt == "create" {
			return ctxResult{expect: eNewName}
		}
		return ctxResult{expect: eRelations | eCTE, scope: scope}
	case "select-list":
		// В списке SELECT после завершения элемента также предлагаем ключевые
		// слова (FROM, ...), чтобы "select id f" вело к FROM, а не к функциям.
		res := ctxResult{expect: eColumns | eFunctions | eKeywords, scope: scope}
		if afterCompleteSelectItem(sig) {
			res.keywords = selectListContinuation
		}
		return res
	case "where", "having", "on", "set", "returning", "groupby", "orderby", "conflict-cols":
		return ctxResult{expect: eColumns | eFunctions | eKeywords, scope: scope}
	case "values":
		return ctxResult{expect: eColumns | eFunctions, scope: scope}
	case "create-kw":
		return ctxResult{keywords: []string{
			"table", "index", "unique index", "view", "materialized view",
			"sequence", "schema", "database", "extension", "function", "role"}}
	case "explain-opts":
		// До PostgreSQL 18: generic_plan (16+), serialize и memory (17+).
		return ctxResult{keywords: []string{
			"analyze", "verbose", "costs", "settings", "buffers", "wal", "timing",
			"summary", "format", "generic_plan", "serialize", "memory"}}
	}

	_ = prevWord
	// Запасной вариант: общие ключевые слова + объекты (но не вываливаем весь
	// каталог как единственный вариант — отношения/функции/столбцы по области плюс
	// ключевые слова).
	if len(scope) > 0 {
		return ctxResult{expect: eColumns | eFunctions | eKeywords, scope: scope}
	}
	return ctxResult{expect: eKeywords | eRelations | eFunctions}
}

func classifyQualified(quals []string, scope []scopeRel, cat *Catalog) ctxResult {
	last := quals[len(quals)-1]
	// schema.relation. -> столбцы ИМЕННО указанной схемы. Решает по ПРЕДпоследнему
	// квалификатору: для "audit.events." это "audit". Явный qualifier должен быть
	// сильнее search_path — иначе при одноимённых таблицах в разных схемах
	// (public.events и audit.events) подсказывались бы колонки не той таблицы.
	// Эту ветку проверяем ПЕРВОЙ, до трактовки last как схемы.
	if cat != nil && len(quals) >= 2 && cat.isSchema(quals[len(quals)-2]) {
		return ctxResult{expect: eColumns, scope: []scopeRel{{schema: quals[len(quals)-2], name: last}}}
	}
	// schema. -> объекты этой схемы
	if cat != nil && len(quals) == 1 && (cat.onSearchPath(last) || cat.isSchema(last)) {
		// schema. -> таблицы/представления и функции этой схемы, упорядоченные по
		// типу, затем по имени. (Вложенных схем нет, поэтому eSchemas намеренно опущен.)
		return ctxResult{expect: eRelations | eFunctions, scope: []scopeRel{{schema: last}}, schemaDrill: true}
	}
	// alias. или relation. -> столбцы разрешённого отношения
	for _, sr := range scope {
		if strings.EqualFold(sr.alias, last) || strings.EqualFold(sr.name, last) {
			return ctxResult{expect: eColumns, scope: []scopeRel{sr}}
		}
	}
	// голое relation. (без псевдонима) -> его столбцы
	return ctxResult{expect: eColumns, scope: []scopeRel{{name: last}}}
}

func (c *Catalog) isSchema(name string) bool {
	c.index()
	if c.schemaSet[name] {
		return true
	}
	for s := range c.schemaSet {
		if strings.EqualFold(s, name) {
			return true
		}
	}
	return false
}

func nonPunct(low []string) []string {
	var out []string
	for _, w := range low {
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

func lastTwo(low []string) (p1, p2 string) {
	words := nonPunct(low)
	if len(words) >= 1 {
		p1 = words[len(words)-1]
	}
	if len(words) >= 2 {
		p2 = words[len(words)-2]
	}
	return
}

func statementKind(low []string) string {
	for _, w := range low {
		switch w {
		case "select", "insert", "update", "delete", "merge", "create", "alter",
			"drop", "truncate", "explain", "with", "grant", "revoke", "copy":
			if w == "with" {
				continue // настоящая инструкция идёт дальше
			}
			return w
		}
	}
	return ""
}

// currentClause находит управляющее ключевое слово предложения, ближайшее к
// курсору на глубине скобок 0, с просмотром двух слов назад для составных предложений.
func currentClause(sig []Token, low []string) (clause, prevWord string) {
	depth := 0
	// Считаем глубину скобок при проходе вперёд, чтобы FROM подзапроса не «утекал».
	depthAt := make([]int, len(sig))
	for i, t := range sig {
		if t.Kind == TPunct && t.Text == "(" {
			depthAt[i] = depth
			depth++
			continue
		}
		if t.Kind == TPunct && t.Text == ")" {
			if depth > 0 {
				depth--
			}
			depthAt[i] = depth
			continue
		}
		depthAt[i] = depth
	}
	cur := depth // текущая глубина скобок у курсора
	for i := len(sig) - 1; i >= 0; i-- {
		if depthAt[i] != cur {
			continue
		}
		w := low[i]
		if w == "" {
			continue
		}
		if i > 0 {
			prevWord = low[i-1]
		}
		switch w {
		case "from":
			return "from", prevWord
		case "join":
			return "join", prevWord
		case "using":
			// Неоднозначно: DELETE FROM t USING <отношение> / MERGE INTO t USING
			// <источник> — позиция ОТНОШЕНИЯ; JOIN ... USING (<столбцы>) — список
			// СТОЛБЦОВ. Различает classify по виду инструкции (stmt).
			return "using", prevWord
		case "into":
			return "into-target", prevWord
		case "update":
			return "update", prevWord
		case "truncate":
			return "truncate", prevWord
		case "references":
			return "references", prevWord
		case "where":
			return "where", prevWord
		case "having":
			return "having", prevWord
		case "on":
			return "on", prevWord
		case "set":
			return "set", prevWord
		case "returning":
			return "returning", prevWord
		case "select":
			return "select-list", prevWord
		case "values":
			return "values", prevWord
		case "by":
			if prevWord == "order" {
				return "orderby", prevWord
			}
			if prevWord == "group" {
				return "groupby", prevWord
			}
		case "table":
			return "table-kw", prevWord
		case "create":
			return "create-kw", prevWord
		case "explain":
			return "explain-opts", prevWord
		case "conflict":
			return "conflict-cols", prevWord
		}
	}
	return "", prevWord
}
