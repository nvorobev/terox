package complete

import (
	"sort"
	"strings"
)

// CandKind классифицирует кандидата автодополнения (для ранжирования и показа).
type CandKind int

const (
	KKeyword CandKind = iota
	KRelation
	KColumn
	KFunction
	KSchema
)

// Candidate — один вариант автодополнения.
type Candidate struct {
	Display  string // отображаемое имя (без кавычек)
	Insert   string // SQL-текст (в кавычках при необходимости); продолжает набранный префикс
	Detail   string // сигнатура / тип / схема
	Coverage string // "[n/m]", если объект есть не на каждом шарде
	Kind     CandKind
	score    int
}

// Result — итог автодополнения: позиция начала замены и ранжированные
// кандидаты (каждый Insert начинается с набранного префикса).
type Result struct {
	ReplaceStart int
	Candidates   []Candidate
}

// explicitKeywordScore ставит грамматически ожидаемые ключевые слова выше функций
// и отношений, чтобы они шли первыми.
const explicitKeywordScore = 45

// topLevelPriority ранжирует основные команды, чтобы по "s" первым шёл SELECT,
// а не более короткий "set". Команды вне списка получают 0.
var topLevelPriority = map[string]int{
	"select": 100, "with": 90, "insert": 80, "update": 70, "delete": 60,
	"create": 50, "alter": 45, "drop": 40, "explain": 35, "merge": 30,
}

// grammarSpecials — SQL-конструкции, которых нет в pg_proc / каталогах;
// предлагаются как кандидаты в выражениях.
var grammarSpecials = []string{
	"case", "cast(", "coalesce(", "nullif(", "greatest(", "least(", "extract(",
	"overlay(", "position(", "substring(", "trim(", "exists(",
	"current_date", "current_timestamp", "current_time", "localtimestamp",
	"current_user", "session_user", "true", "false", "null",
}

// analyze разбирает head на токены и классифицирует контекст в конце head.
// suppressed=true внутри строки/комментария (дополнение отключено). scopeText —
// текст для сбора отношений/алиасов в области видимости: обычно равен head, но в
// CompleteAt это ВЕСЬ запрос, чтобы FROM/алиас после курсора (например
// "select i.| from t i") тоже разрешался.
func analyze(head, scopeText string, cat *Catalog) (replaceStart int, partial string, ctx ctxResult, suppressed bool) {
	switch TrailingStateOf(head) {
	case StateString, StateComment:
		return len(head), "", ctxResult{}, true
	}
	toks := Lex(head)
	sig := significant(toks)
	scopeSig := sig
	if scopeText != head {
		scopeSig = significant(Lex(scopeText))
	}
	rs, p, quals := analyzePrefix(toks, head)
	return rs, p, classify(sig, scopeSig, p != "", quals, cat), false
}

// Complete возвращает ранжированные дополнения для head (текста слева от курсора).
func Complete(head string, cat *Catalog) Result { return completeFrom(head, head, cat) }

// CompleteAt — это Complete с учётом курсора: head равен line[:pos], а область
// видимости (алиас→отношение) собирается со всей строки, поэтому колонки
// дополняются после алиаса, даже если его FROM находится СПРАВА от курсора.
func CompleteAt(line string, pos int, cat *Catalog) Result {
	if pos < 0 {
		pos = 0
	}
	if pos > len(line) {
		pos = len(line)
	}
	return completeFrom(line[:pos], line, cat)
}

func completeFrom(head, scopeText string, cat *Catalog) Result {
	if cat == nil {
		cat = &Catalog{}
	}
	replaceStart, partial, ctx, suppressed := analyze(head, scopeText, cat)
	if suppressed {
		return Result{ReplaceStart: replaceStart}
	}
	cands := generate(ctx, cat, partial)
	cands = rank(cands, partial)
	if ctx.schemaDrill {
		// "schema." → сортировка строго по типу объекта, затем по имени (таблицы,
		// представления, …, затем функции), а не по оценке релевантности.
		sortByTypeName(cands)
	}
	return Result{ReplaceStart: replaceStart, Candidates: cands}
}

// relKindOrder задаёт порядок видов отношений при показе drill по схеме: сначала
// таблицы, затем представления/матвью, затем остальное.
var relKindOrder = map[string]int{
	"table": 0, "partitioned": 1, "view": 2, "matview": 3,
	"foreign": 4, "sequence": 5, "index": 6,
}

// sortByTypeName сортирует кандидатов по типу объекта (отношения по relkind,
// затем функции, затем схемы) и по алфавиту внутри каждого типа.
func sortByTypeName(c []Candidate) {
	order := func(cd Candidate) (int, int) {
		switch cd.Kind {
		case KRelation:
			kind := cd.Detail
			if i := strings.IndexByte(kind, ' '); i >= 0 {
				kind = kind[:i] // Detail имеет вид "<relkind> <schema>"
			}
			o, ok := relKindOrder[kind]
			if !ok {
				o = 9
			}
			return 0, o
		case KFunction:
			return 1, 0
		case KSchema:
			return 2, 0
		default:
			return 3, 0
		}
	}
	sort.SliceStable(c, func(i, j int) bool {
		gi, si := order(c[i])
		gj, sj := order(c[j])
		if gi != gj {
			return gi < gj
		}
		if si != sj {
			return si < sj
		}
		return strings.ToLower(c[i].Display) < strings.ToLower(c[j].Display)
	})
}

// NeededColumns возвращает отношения, чьи колонки понадобятся дополнению в head
// (разрешённые через search_path), чтобы хост мог лениво загрузить их перед
// вызовом Complete. Пусто, если контексту колонки не нужны.
func NeededColumns(head string, cat *Catalog) []RelRef {
	return neededColumnsFrom(head, head, cat)
}

// NeededColumnsAt — это NeededColumns с учётом курсора (область видимости со всей
// строки), как в CompleteAt.
func NeededColumnsAt(line string, pos int, cat *Catalog) []RelRef {
	if pos < 0 {
		pos = 0
	}
	if pos > len(line) {
		pos = len(line)
	}
	return neededColumnsFrom(line[:pos], line, cat)
}

func neededColumnsFrom(head, scopeText string, cat *Catalog) []RelRef {
	if cat == nil {
		return nil
	}
	_, _, ctx, suppressed := analyze(head, scopeText, cat)
	if suppressed || ctx.expect&eColumns == 0 {
		return nil
	}
	var out []RelRef
	seen := map[string]bool{}
	for _, sr := range ctx.scope {
		if sr.name == "" || sr.isCTE || sr.isDerived {
			continue // схема / CTE / производная таблица: колонок в information_schema нет
		}
		schema, name := sr.schema, sr.name
		if schema == "" {
			rels := cat.resolveRelations(name)
			if len(rels) == 0 {
				continue // не разрешено (алиас подзапроса / CTE) — нечего загружать
			}
			schema, name = rels[0].Schema, rels[0].Name
		}
		key := schema + "\x00" + name
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, RelRef{Schema: schema, Name: name})
	}
	return out
}

// generate формирует исходный набор кандидатов для классифицированного контекста.
func generate(ctx ctxResult, cat *Catalog, partial string) []Candidate {
	cat.index()
	quoted := strings.HasPrefix(partial, `"`)
	insertIdent := func(name string) string {
		if quoted {
			return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		}
		return QuoteIdent(name, cat.Reserved)
	}

	var out []Candidate
	add := func(c Candidate) { out = append(out, c) }

	if ctx.expect&eNewName != 0 {
		return nil // ожидается новый идентификатор; из каталога ничего не предлагаем
	}

	// Явный набор ключевых слов (грамматически ожидаемые следующие слова). Они
	// ранжируются ВЫШЕ функций — "f" после завершённого элемента дополняется до
	// FROM, а не до floor().
	if len(ctx.keywords) > 0 {
		for _, kw := range ctx.keywords {
			add(Candidate{Display: kw, Insert: kw, Kind: KKeyword, score: explicitKeywordScore})
		}
	}

	if ctx.expect&eTopLevel != 0 {
		for _, kw := range topLevelCommands {
			add(Candidate{Display: kw, Insert: kw, Kind: KKeyword, score: topLevelPriority[kw]})
		}
		return out
	}

	if ctx.expect&eSchemas != 0 {
		for _, s := range cat.Schemas {
			if !cat.IncludeSystem && IsSystemSchema(s) {
				continue // pg_catalog/information_schema скрыты вне системного режима
			}
			add(Candidate{Display: s, Insert: insertIdent(s), Kind: KSchema, Detail: "schema"})
		}
	}

	// Имена CTE из запроса (позиция FROM): предлагаем CTE, объявленные в WITH.
	if ctx.expect&eCTE != 0 {
		seenCTE := map[string]bool{}
		for _, sr := range ctx.scope {
			if sr.isCTE && sr.name != "" && !seenCTE[sr.name] {
				seenCTE[sr.name] = true
				add(Candidate{Display: sr.name, Insert: insertIdent(sr.name), Kind: KRelation, Detail: "CTE", score: 60})
			}
		}
	}

	if ctx.expect&eRelations != 0 {
		fromPos := ctx.expect&eCTE != 0 // FROM/JOIN/цель: только выбираемые отношения
		// Если задана одна схема (квалификатор schema.), ограничиваемся ею.
		var rels []Relation
		schemaScoped := len(ctx.scope) == 1 && ctx.scope[0].schema != "" && ctx.scope[0].name == ""
		if schemaScoped {
			rels = cat.relationsInSchema(ctx.scope[0].schema)
		} else {
			rels = cat.Relations
		}
		for _, r := range rels {
			// И FROM/JOIN, и drill по "schema." нужны только выбираемые отношения
			// (таблицы, представления, матвью, секционированные, внешние) — без
			// индексов, первичных ключей и последовательностей, которые лишь
			// засоряют выбор объекта для запроса.
			if (fromPos || schemaScoped) && !r.SelectableKind() {
				continue
			}
			if !schemaScoped && !cat.IncludeSystem {
				// Скрываем таблицы/представления системного каталога из общего списка.
				if IsSystemSchema(r.Schema) {
					continue
				}
				// Имя без квалификатора разрешается только для отношений, чья схема
				// есть в search_path. Таблицу из другой схемы можно записать ТОЛЬКО
				// как "schema.table", поэтому её голое имя здесь не предлагаем — вместо
				// этого предлагается схема (eSchemas), и в неё заходят через ".".
				if !cat.onSearchPath(r.Schema) {
					continue
				}
			}
			cov := cat.coverageBadge("rel:" + r.Schema + "." + r.Name)
			add(Candidate{
				Display: r.Name, Insert: insertIdent(r.Name), Kind: KRelation,
				Detail: relKindName(r.Kind) + " " + r.Schema, Coverage: cov,
				score: schemaBonus(cat, r.Schema),
			})
		}
	}

	if ctx.expect&eColumns != 0 {
		seen := map[string]bool{}
		emit := func(col Column, local bool) {
			key := strings.ToLower(col.Name)
			if seen[key] {
				return
			}
			seen[key] = true
			bonus := 0
			if local {
				bonus += 50
			}
			cov := cat.coverageBadge("col:" + col.Schema + "." + col.Relation + "." + col.Name)
			add(Candidate{
				Display: col.Name, Insert: insertIdent(col.Name), Kind: KColumn,
				Detail: col.Type, Coverage: cov, score: bonus,
			})
		}
		if len(ctx.scope) > 0 {
			for _, sr := range ctx.scope {
				for _, col := range cat.columnsOf(sr.schema, sr.name) {
					emit(col, true)
				}
				// Производная таблица (подзапрос в FROM) не имеет записи в каталоге —
				// предлагаем имена её выводимых колонок, разобранные из списка SELECT.
				for _, name := range sr.derivedCols {
					emit(Column{Name: name}, true)
				}
			}
		}
		if len(ctx.scope) == 0 && partial != "" {
			// Нет разрешённой области ("SELECT col" без FROM): выдаём только колонки,
			// совпадающие с набранным префиксом. Выдавать все колонки каталога на
			// каждое нажатие расточительно, rank() всё равно отбросит несовпадающие,
			// а пустой префикс не может осмысленно перечислить всю БД.
			// Снимаем ведущую кавычку из префикса, чтобы quoted-ввод ("Co) не отсекал
			// колонки здесь; точную регистрозависимость для quoted применяет rank().
			lp := strings.ToLower(strings.TrimPrefix(partial, `"`))
			for _, col := range cat.Columns {
				if strings.HasPrefix(strings.ToLower(col.Name), lp) {
					emit(col, false)
				}
			}
		}
	}

	if ctx.expect&eFunctions != 0 {
		fromPos := ctx.expect&eCTE != 0 // FROM: только табличные (set-returning) функции
		// Drill по "schema." ограничивает функции этой схемой; иначе предлагается весь
		// вызываемый набор (включая встроенные pg_catalog вроде now()). Встроенные
		// функции не закрываются системным режимом — это реальные функции запросов.
		var funcs []Function
		if len(ctx.scope) == 1 && ctx.scope[0].schema != "" && ctx.scope[0].name == "" {
			funcs = cat.functionsInSchema(ctx.scope[0].schema)
		} else {
			funcs = cat.Functions
		}
		schemaScoped := len(ctx.scope) == 1 && ctx.scope[0].schema != "" && ctx.scope[0].name == ""
		for _, f := range funcs {
			if fromPos && !f.RetSet {
				continue
			}
			// Пустой Tab не засоряем: встроенные функции (pg_catalog/...) показываются
			// только после набранного префикса ("gen"+Tab → generate_series,
			// "no"+Tab → now()), если только не включён системный режим или это явный
			// drill по схеме. Функции из схем пользователя показываются всегда.
			// Работает и в FROM, и в позиции выражения, чтобы пустой Tab не утонул в
			// сотнях функций pg_catalog.
			if partial == "" && !schemaScoped && !cat.IncludeSystem && IsSystemSchema(f.Schema) {
				continue
			}
			add(Candidate{
				Display: f.Name, Insert: funcInsert(f, insertIdent), Kind: KFunction,
				Detail: f.Signature, score: schemaBonus(cat, f.Schema),
			})
		}
	}

	if ctx.expect&eKeywords != 0 {
		for _, kw := range grammarSpecials {
			add(Candidate{Display: kw, Insert: kw, Kind: KKeyword})
		}
		for _, kw := range cat.Keywords {
			add(Candidate{Display: kw, Insert: kw, Kind: KKeyword})
		}
	}
	return out
}

func funcInsert(f Function, insertIdent func(string) string) string {
	name := insertIdent(f.Name)
	if f.NoParen {
		return name
	}
	if f.MinArgs == 0 {
		return name + "()"
	}
	return name + "("
}

func schemaBonus(cat *Catalog, schema string) int {
	// Системные объекты доступны для поиска, но редко нужны пользователю первыми.
	if schema == "pg_catalog" || schema == "information_schema" {
		return 0
	}
	if cat.onSearchPath(schema) {
		// чем раньше в (пользовательской части) search_path, тем выше ранг
		if r, ok := cat.inPathRank[schema]; ok {
			return 30 - r
		}
		return 20
	}
	return 5 // именованная несистемная схема всё равно выше системных объектов
}

func relKindName(k string) string {
	switch k {
	case "r":
		return "table"
	case "v":
		return "view"
	case "m":
		return "matview"
	case "p":
		return "partitioned"
	case "f":
		return "foreign"
	case "i":
		return "index"
	case "S":
		return "sequence"
	}
	return "relation"
}

// rank оставляет кандидатов, чей Insert продолжает набранный префикс (без учёта
// регистра — этого требует модель дополнения только дописыванием), применяет
// политику регистра ключевых слов, считает оценку, сортирует и убирает дубликаты.
func rank(cands []Candidate, partial string) []Candidate {
	lp := strings.ToLower(partial)
	upper := isUpper(partial)
	// Префикс в двойных кавычках ("Foo) — это регистрозависимый идентификатор в
	// PostgreSQL, поэтому он должен совпадать только с кандидатом точного регистра
	// ("Foo", но не foo/FOO).
	quoted := strings.HasPrefix(partial, `"`)
	out := make([]Candidate, 0, len(cands))
	seen := map[string]bool{}
	for _, c := range cands {
		ins := c.Insert
		// Политика регистра ключевых слов: следуем набранному регистру (SEL -> SELECT).
		if c.Kind == KKeyword && upper {
			ins = strings.ToUpper(ins)
			c.Insert = ins
			c.Display = strings.ToUpper(c.Display)
		}
		if quoted {
			if !strings.HasPrefix(ins, partial) { // точный регистр, с кавычкой
				continue
			}
		} else if !strings.HasPrefix(strings.ToLower(ins), lp) {
			continue
		}
		dedupe := string(rune(c.Kind)) + "\x00" + ins
		if seen[dedupe] {
			continue
		}
		seen[dedupe] = true
		// Оценка: бонус области/схемы (уже в c.score) + точный регистр + вид.
		s := c.score
		if strings.HasPrefix(ins, partial) {
			s += 5 // префикс точного регистра
		}
		// Точное совпадение имени побеждает независимо от вида: полностью набранное
		// имя схемы выводит её (для drill через ".") выше таблицы, лишь делящей
		// префикс.
		if partial != "" && strings.EqualFold(c.Display, partial) {
			s += 100
		}
		switch c.Kind {
		case KColumn:
			s += 8
		case KRelation:
			s += 6
		case KFunction:
			s += 4
		case KSchema:
			s += 5 // навигация от схем: схемы остаются конкурентоспособны в FROM
		}
		c.score = s
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		if len(out[i].Display) != len(out[j].Display) {
			return len(out[i].Display) < len(out[j].Display)
		}
		return out[i].Display < out[j].Display
	})
	return out
}

func isUpper(s string) bool {
	has := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			return false
		}
		if c >= 'A' && c <= 'Z' {
			has = true
		}
	}
	return has
}
