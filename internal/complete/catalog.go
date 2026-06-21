package complete

import (
	"sort"
	"strings"

	"terox/internal/catalog"
)

// Модель состояния сегментов каталога перенесена в internal/catalog (ARCH из
// аудита, раздел 4.2). Здесь оставлены алиасы и ре-экспорт констант, чтобы все
// существующие ссылки complete.Status*/complete.LoadState продолжали работать.

// Status — состояние загрузки сегмента каталога.
type Status = catalog.Status

const (
	StatusPending   = catalog.StatusPending
	StatusLoaded    = catalog.StatusLoaded
	StatusPartial   = catalog.StatusPartial
	StatusForbidden = catalog.StatusForbidden
	StatusTimeout   = catalog.StatusTimeout
	StatusFailed    = catalog.StatusFailed
)

// LoadState — состояние одного сегмента каталога.
type LoadState = catalog.LoadState

// Relation — типизированная идентичность отношения (с указанием схемы).
type Relation struct {
	Schema string
	Name   string
	Kind   string // r таблица, v представление, m matview, p секционированная, f сторонняя, i индекс, S последовательность
}

// SelectableKind сообщает, может ли отношение появляться в FROM/JOIN.
func (r Relation) SelectableKind() bool {
	switch r.Kind {
	case "r", "v", "m", "p", "f":
		return true
	}
	return false
}

// Column — типизированная идентичность столбца.
type Column struct {
	Schema, Relation, Name, Type string
}

// RelRef — разрешённая ссылка (schema, relation); сообщает хосту, столбцы каких
// отношений нужны для дополнения, чтобы он мог загрузить их лениво.
type RelRef struct {
	Schema, Name string
}

// Function — типизированная идентичность функции с отрисованной сигнатурой.
type Function struct {
	Schema    string
	Name      string
	Signature string // напр. "(integer, integer)" — для отображения
	Kind      string // f функция, a агрегат, w оконная, p процедура
	MinArgs   int    // минимальное число аргументов среди перегрузок (0 => вызывается без аргументов)
	NoParen   bool   // нульарный SQL-спецсимвол (current_user) — вставляется без "()"
	RetSet    bool   // возвращает множество (proretset) — применима в FROM (generate_series, unnest, ...)
}

// Catalog — типизированный снимок дополняемых объектов БД с сохранением
// идентичности схем. Содержит порядок search_path и, для целей из нескольких
// шардов, счётчики покрытия по объектам.
type Catalog struct {
	SearchPath []string // разрешённые имена схем в порядке приоритета
	Schemas    []string
	Relations  []Relation
	Columns    []Column
	Functions  []Function
	// Extensions — установленные расширения (pg_extension). Полезно знать наличие
	// pg_stat_statements/pgcrypto/… и видеть дрейф между шардами.
	Extensions []string
	// Enums — имена enum-типов (schema.name), для будущего дополнения значений и
	// диагностики покрытия типов.
	Enums    []string
	Keywords []string        // все ключевые слова сервера (для общего дополнения)
	Reserved map[string]bool // ключевые слова, требующие кавычек при использовании как имя
	Shards   int             // число шардов в этом снимке (>=1)
	Coverage map[string]int  // ключ объекта -> число шардов, где он есть
	// Segments — состояние загрузки по сегментам каталога (relations, schemas,
	// search_path, keywords, functions, coverage). Пусто = состояния не отслеживались.
	Segments map[string]LoadState

	// IncludeSystem при true разрешает дополнению предлагать объекты системного
	// каталога (таблицы и схемы pg_catalog / information_schema / pg_toast / pg_temp).
	// При false список предложений ограничен объектами БД пользователя, чтобы Tab
	// не вываливал десятки системных таблиц. Встроенные ФУНКЦИИ (now(), count(), ...)
	// предлагаются всегда независимо от флага. Хост задаёт его из переключателя
	// \completion system перед каждым вызовом Complete.
	IncludeSystem bool

	// лениво строящиеся индексы
	relByName  map[string][]Relation
	colByRel   map[string][]Column // ключ: schema\x00relation
	colByName  map[string][]Column // имя отношения (любая схема) -> столбцы
	schemaSet  map[string]bool
	inPathRank map[string]int
	loaded     map[string]bool // relKey -> столбцы загружены (ленивая загрузка)
}

func relKey(schema, relation string) string { return schema + "\x00" + relation }

func (c *Catalog) index() {
	if c.relByName != nil {
		return
	}
	c.relByName = map[string][]Relation{}
	c.colByRel = map[string][]Column{}
	c.colByName = map[string][]Column{}
	c.schemaSet = map[string]bool{}
	c.inPathRank = map[string]int{}
	c.loaded = map[string]bool{}
	for _, s := range c.Schemas {
		c.schemaSet[s] = true
	}
	for i, s := range c.SearchPath {
		if _, ok := c.inPathRank[s]; !ok {
			c.inPathRank[s] = i
		}
	}
	for _, r := range c.Relations {
		// Ключ в нижнем регистре: resolveRelations ищет по strings.ToLower(name),
		// поэтому отношение со смешанным регистром индексируется в нижнем регистре.
		k := strings.ToLower(r.Name)
		c.relByName[k] = append(c.relByName[k], r)
	}
	for _, col := range c.Columns {
		c.colByRel[relKey(col.Schema, col.Relation)] = append(c.colByRel[relKey(col.Schema, col.Relation)], col)
		c.colByName[strings.ToLower(col.Relation)] = append(c.colByName[strings.ToLower(col.Relation)], col)
		c.loaded[relKey(col.Schema, col.Relation)] = true
	}
}

// Index заранее строит индексы поиска. Хост вызывает это один раз перед
// публикацией каталога для конкурентных читателей, чтобы последующий доступ
// только для чтения из горутины не запускал ленивую несинхронизированную сборку,
// конкурирующую с SetColumns. Можно вызывать многократно.
func (c *Catalog) Index() { c.index() }

// ColumnsLoaded сообщает, загружены ли столбцы (schema, relation). При пустой
// схеме она разрешается через search_path.
func (c *Catalog) ColumnsLoaded(schema, relation string) bool {
	c.index()
	if schema == "" {
		rels := c.resolveRelations(relation)
		if len(rels) == 0 {
			return false
		}
		schema, relation = rels[0].Schema, rels[0].Name
	}
	return c.loaded[relKey(schema, relation)]
}

// SetColumns помещает лениво загруженные столбцы (schema, relation) в индексы
// каталога и помечает их как загруженные.
func (c *Catalog) SetColumns(schema, relation string, cols []Column) {
	c.index()
	key := relKey(schema, relation)
	if c.loaded[key] {
		return
	}
	c.loaded[key] = true
	for _, col := range cols {
		c.Columns = append(c.Columns, col)
		c.colByRel[relKey(col.Schema, col.Relation)] = append(c.colByRel[relKey(col.Schema, col.Relation)], col)
		c.colByName[strings.ToLower(col.Relation)] = append(c.colByName[strings.ToLower(col.Relation)], col)
	}
}

// onSearchPath сообщает, находится ли схема в search_path.
func (c *Catalog) onSearchPath(schema string) bool {
	c.index()
	_, ok := c.inPathRank[schema]
	return ok
}

// HasSchema сообщает, является ли name известной схемой.
func (c *Catalog) HasSchema(name string) bool {
	c.index()
	return c.schemaSet[name]
}

// HasRelation сообщает, является ли (schema, name) известным отношением. При
// пустой схеме имя разрешается через search_path.
func (c *Catalog) HasRelation(schema, name string) bool {
	c.index()
	if schema == "" {
		return len(c.resolveRelations(name)) > 0
	}
	for _, r := range c.Relations {
		if strings.EqualFold(r.Schema, schema) && strings.EqualFold(r.Name, name) {
			return true
		}
	}
	return false
}

// IsSystemSchema сообщает, является ли схема системным каталогом PostgreSQL
// (её таблицы/представления — служебные, не пользовательские данные). Такие
// схемы скрыты из дополнения, если не задан IncludeSystem.
func IsSystemSchema(schema string) bool {
	return schema == "pg_catalog" || schema == "information_schema" ||
		strings.HasPrefix(schema, "pg_toast") || strings.HasPrefix(schema, "pg_temp")
}

// RelationArgCandidates возвращает дополнения имён таблиц для табличного
// аргумента метакоманды (\count, \locate, \diff, \d): имена схем для углубления,
// короткие имена выбираемых отношений, чья схема в search_path, и имена с
// указанием схемы для остальных. Системные схемы исключены. Используется metaArgs.
func RelationArgCandidates(c *Catalog) []string {
	if c == nil {
		return nil
	}
	c.index()
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range c.Schemas {
		if !c.IncludeSystem && IsSystemSchema(s) {
			continue
		}
		add(QuoteIdent(s, c.Reserved))
	}
	for _, r := range c.Relations {
		if !c.IncludeSystem && IsSystemSchema(r.Schema) {
			continue
		}
		if !r.SelectableKind() {
			continue
		}
		add(QuoteQualified(r.Schema, r.Name, c.Reserved)) // schema.name
		// Короткое имя, когда оно разрешается в search_path; в системном режиме
		// имена pg_catalog тоже даются коротко, так как видны везде.
		if c.IncludeSystem || c.onSearchPath(r.Schema) {
			add(QuoteIdent(r.Name, c.Reserved))
		}
	}
	return out
}

// relationsInSchema возвращает отношения заданной схемы.
func (c *Catalog) relationsInSchema(schema string) []Relation {
	var out []Relation
	for _, r := range c.Relations {
		if strings.EqualFold(r.Schema, schema) {
			out = append(out, r)
		}
	}
	return out
}

// functionsInSchema возвращает функции заданной схемы.
func (c *Catalog) functionsInSchema(schema string) []Function {
	var out []Function
	for _, f := range c.Functions {
		if strings.EqualFold(f.Schema, schema) {
			out = append(out, f)
		}
	}
	return out
}

// resolveRelations возвращает отношения с именем `name`, соблюдая порядок
// search_path (первое совпадение — то, что выбрал бы PostgreSQL без указания схемы).
func (c *Catalog) resolveRelations(name string) []Relation {
	c.index()
	rels := append([]Relation(nil), c.relByName[strings.ToLower(name)]...)
	// запасной вариант без учёта регистра
	if len(rels) == 0 {
		for _, r := range c.Relations {
			if strings.EqualFold(r.Name, name) {
				rels = append(rels, r)
			}
		}
	}
	sort.SliceStable(rels, func(i, j int) bool {
		ri, oki := c.inPathRank[rels[i].Schema]
		rj, okj := c.inPathRank[rels[j].Schema]
		if oki != okj {
			return oki // схема из search_path идёт первой
		}
		return ri < rj
	})
	return rels
}

// columnsOf возвращает столбцы конкретного schema.relation.
func (c *Catalog) columnsOf(schema, relation string) []Column {
	c.index()
	if schema == "" {
		// разрешить по имени через search_path
		rels := c.resolveRelations(relation)
		if len(rels) > 0 {
			return c.colByRel[relKey(rels[0].Schema, rels[0].Name)]
		}
		return c.colByName[strings.ToLower(relation)]
	}
	out := c.colByRel[relKey(schema, relation)]
	if out == nil {
		// без учёта регистра
		for _, col := range c.Columns {
			if strings.EqualFold(col.Schema, schema) && strings.EqualFold(col.Relation, relation) {
				out = append(out, col)
			}
		}
	}
	return out
}

// coverageBadge отрисовывает покрытие объекта по шардам, когда он есть не на
// всех шардах, иначе "".
func (c *Catalog) coverageBadge(key string) string {
	if c.Shards <= 1 || c.Coverage == nil {
		return ""
	}
	n, ok := c.Coverage[key]
	if !ok || n >= c.Shards {
		return ""
	}
	return "[" + itoa(n) + "/" + itoa(c.Shards) + "]"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
