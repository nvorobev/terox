package complete

import "strings"

// RelationIssue — ссылка на отношение из FROM/JOIN/USING/UPDATE/INTO, которая не
// разрешается по снимку каталога (каталого-зависимый lint, Feature 6+).
//   - "unknown-relation": имя не найдено в каталоге;
//   - "unknown-schema": указанная схема неизвестна.
type RelationIssue struct {
	Schema string
	Name   string
	Reason string
}

// Qualified отдаёт ссылку как пользователь её написал (schema.name или name).
func (i RelationIssue) Qualified() string {
	if i.Schema != "" {
		return i.Schema + "." + i.Name
	}
	return i.Name
}

// LintRelations проверяет ссылки на отношения в sql по снимку каталога и
// возвращает те, что не разрешаются. Каталого-зависимая проверка дополняет
// статический diag (Feature 6+). Консервативна по построению — линтер с ложными
// срабатываниями хуже отсутствия линтера:
//
//   - при пустом/незагруженном каталоге не диагностирует ничего (нечем сверять);
//   - пропускает CTE и производные таблицы (их в каталоге нет по определению);
//   - пропускает вызовы функций, включая табличные в FROM (имя сразу перед "(");
//   - имя считается неизвестным, ТОЛЬКО если его нет в каталоге нигде — отношение
//     вне search_path тоже считается «найдено», чтобы не ругаться на существующее.
func LintRelations(sql string, cat *Catalog) []RelationIssue {
	if cat == nil || len(cat.Relations) == 0 {
		return nil
	}
	sig := significant(Lex(sql))
	scope := gatherScope(sig)
	if len(scope) == 0 {
		return nil
	}
	// Имена, за которыми сразу идёт "(" — вызовы функций (в т.ч. табличные в FROM),
	// а не отношения; не диагностируем.
	funcCall := map[string]bool{}
	for i := 0; i+1 < len(sig); i++ {
		if (sig[i].Kind == TWord || sig[i].Kind == TQIdent) &&
			sig[i+1].Kind == TPunct && sig[i+1].Text == "(" {
			funcCall[strings.ToLower(identName(sig[i]))] = true
		}
	}
	cteNames := map[string]bool{}
	for _, sr := range scope {
		if sr.isCTE {
			cteNames[strings.ToLower(sr.name)] = true
		}
	}
	var out []RelationIssue
	seen := map[string]bool{}
	for _, sr := range scope {
		if sr.isCTE || sr.isDerived || sr.name == "" {
			continue
		}
		lname := strings.ToLower(sr.name)
		if cteNames[lname] || funcCall[lname] {
			continue
		}
		key := strings.ToLower(sr.schema) + "." + lname
		if seen[key] {
			continue
		}
		seen[key] = true
		if sr.schema != "" {
			switch {
			case !cat.isSchema(sr.schema):
				out = append(out, RelationIssue{Schema: sr.schema, Name: sr.name, Reason: "unknown-schema"})
			case !cat.HasRelation(sr.schema, sr.name):
				out = append(out, RelationIssue{Schema: sr.schema, Name: sr.name, Reason: "unknown-relation"})
			}
			continue
		}
		if !cat.HasRelation("", sr.name) {
			out = append(out, RelationIssue{Name: sr.name, Reason: "unknown-relation"})
		}
	}
	return out
}
