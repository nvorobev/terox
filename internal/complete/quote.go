package complete

import "strings"

// builtinReserved — запасной набор зарезервированных ключевых слов SQL,
// которые нужно брать в двойные кавычки при использовании как идентификатор.
// Применяется, когда каталог не отдаёт свой набор Reserved (например, в тестах).
var builtinReserved = map[string]bool{
	"all": true, "analyse": true, "analyze": true, "and": true, "any": true,
	"array": true, "as": true, "asc": true, "asymmetric": true, "authorization": true,
	"between": true, "binary": true, "both": true, "case": true, "cast": true,
	"check": true, "collate": true, "collation": true, "column": true, "concurrently": true,
	"constraint": true, "create": true, "cross": true, "current_catalog": true,
	"current_date": true, "current_role": true, "current_schema": true, "current_time": true,
	"current_timestamp": true, "current_user": true, "default": true, "deferrable": true,
	"desc": true, "distinct": true, "do": true, "else": true, "end": true, "except": true,
	"false": true, "fetch": true, "for": true, "foreign": true, "freeze": true, "from": true,
	"full": true, "grant": true, "group": true, "having": true, "ilike": true, "in": true,
	"initially": true, "inner": true, "intersect": true, "into": true, "is": true, "isnull": true,
	"join": true, "lateral": true, "leading": true, "left": true, "like": true, "limit": true,
	"localtime": true, "localtimestamp": true, "natural": true, "not": true, "notnull": true,
	"null": true, "offset": true, "on": true, "only": true, "or": true, "order": true,
	"outer": true, "overlaps": true, "placing": true, "primary": true, "references": true,
	"returning": true, "right": true, "select": true, "session_user": true, "similar": true,
	"some": true, "symmetric": true, "system_user": true, "table": true, "tablesample": true,
	"then": true, "to": true, "trailing": true, "true": true, "union": true, "unique": true,
	"user": true, "using": true, "variadic": true, "verbose": true, "when": true, "where": true,
	"window": true, "with": true,
}

// needsQuote сообщает, нужно ли брать name в двойные кавычки: это не простой
// идентификатор из строчных букв либо это зарезервированное слово.
func needsQuote(name string, reserved map[string]bool) bool {
	if name == "" {
		return true
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if i == 0 {
			if !(c == '_' || (c >= 'a' && c <= 'z')) {
				return true // заглавная буква, цифра, '$', Unicode и т.п.
			}
			continue
		}
		if !(c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return true
		}
	}
	lower := strings.ToLower(name)
	if reserved != nil && reserved[lower] {
		return true
	}
	return builtinReserved[lower]
}

// QuoteIdent возвращает name в виде, пригодном для SQL: без кавычек для простого
// идентификатора, иначе — в двойных кавычках с удвоением внутренних кавычек,
// сохраняя регистр.
func QuoteIdent(name string, reserved map[string]bool) string {
	if !needsQuote(name, reserved) {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// QuoteQualified формирует имя с возможной квалификацией по схеме, заключая
// каждую часть в кавычки при необходимости: ("public","Order Items") -> public."Order Items".
func QuoteQualified(schema, name string, reserved map[string]bool) string {
	q := QuoteIdent(name, reserved)
	if schema == "" {
		return q
	}
	return QuoteIdent(schema, reserved) + "." + q
}
