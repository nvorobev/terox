package repl

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"terox/internal/cluster"
	"terox/internal/complete"
	"terox/internal/db"
	"terox/internal/render"
	"terox/internal/sqlsplit"
)

// fanoutRead выполняет read-only запрос по всем текущим целям, с прогрессом
// и отменой по Ctrl-C для множества шардов.
func (r *REPL) fanoutRead(sql string) []db.ShardResult {
	return r.fanout(r.targets, sql, true, time.Duration(r.cfg.QueryTimeout))
}

// completeCatalog возвращает текущий каталог автодополнения (может быть nil во
// время фоновой загрузки) и запускает загрузку при необходимости. Не блокирует
// на I/O БД, поэтому отрисовка Tab/ghost мгновенна.
func (r *REPL) completeCatalog() *complete.Catalog {
	r.catalogMu.Lock()
	c := r.catalog
	r.catalogMu.Unlock()
	r.kickCatalog()
	return c
}

// kickCatalog запускает ФОНОВУЮ сборку каталога, если он не загружен и не
// строится (с коротким backoff после неудачи). Не блокирует. Загрузка помечается
// текущей эпохой: при смене контекста её результат отбрасывается, а не затирает
// каталог нового контекста.
func (r *REPL) kickCatalog() {
	r.catalogMu.Lock()
	if r.catalog != nil || r.catalogLoading || len(r.targets) == 0 ||
		(!r.catalogAttempt.IsZero() && time.Since(r.catalogAttempt) < 3*time.Second) {
		r.catalogMu.Unlock()
		return
	}
	r.catalogLoading = true
	r.catalogAttempt = time.Now()
	epoch := r.catalogEpoch
	targets := append([]cluster.Shard(nil), r.targets...)
	mgr, conc := r.mgr, r.cfg.ProbeConcurrency(len(r.targets))
	r.catalogMu.Unlock()

	go func() {
		cat, err := buildCatalog(mgr, targets, conc)
		if cat != nil {
			// Строим индексы до публикации, чтобы путь автодополнения мета-команд
			// (без блокировки) не запускал ленивую сборку, гоняющуюся с SetColumns
			// под блокировкой.
			cat.Index()
		}
		r.catalogMu.Lock()
		if epoch == r.catalogEpoch { // контекст не менялся — принимаем результат
			r.catalog = cat // nil при ошибке -> повтор после backoff
			r.catalogErr = err
			r.catalogLoading = false
			// Если каталог загрузился с деградировавшими сегментами, готовим
			// одноразовую заметку — основной цикл покажет её перед приглашением,
			// чтобы пользователь сразу узнал о partial-дополнении (P2-5).
			if cat != nil {
				r.catalogNotice = degradedNotice(cat.Segments)
			}
		}
		r.catalogMu.Unlock()
	}()
}

// resetCatalog сбрасывает каталог при смене контекста и инвалидирует фоновую
// загрузку (через эпоху), чтобы она не установила устаревший снимок.
func (r *REPL) resetCatalog() {
	r.catalogMu.Lock()
	r.catalog = nil
	r.catalogLoading = false
	r.catalogEpoch++
	r.catalogAttempt = time.Time{}
	r.catalogErr = nil
	r.catalogNotice = ""
	r.colFetching = nil
	r.pkCache = nil
	r.pkFetching = nil
	r.serverVer = 0
	r.catalogMu.Unlock()
}

// loadColumns синхронно загружает колонки отношения (явный Tab) и помещает их в
// cat (под блокировкой, только если cat ещё актуален).
func (r *REPL) loadColumns(cat *complete.Catalog, targets []cluster.Shard, conc int, ref complete.RelRef) {
	cols, cov := fetchColumns(r.mgr, targets, conc, ref.Schema, ref.Name)
	r.catalogMu.Lock()
	if r.catalog == cat {
		cat.SetColumns(ref.Schema, ref.Name, cols)
		mergeCoverage(cat, cov)
	}
	r.catalogMu.Unlock()
}

// loadColumnsAsync загружает колонки отношения в фоне (ghost на каждое нажатие),
// чтобы UI не блокировался; результаты появляются на следующей отрисовке.
// Дедуплицирует параллельные загрузки одного отношения.
func (r *REPL) loadColumnsAsync(cat *complete.Catalog, targets []cluster.Shard, conc int, ref complete.RelRef) {
	key := ref.Schema + "\x00" + ref.Name
	r.catalogMu.Lock()
	if cat != r.catalog || r.colFetching[key] {
		r.catalogMu.Unlock()
		return
	}
	if r.colFetching == nil {
		r.colFetching = map[string]bool{}
	}
	r.colFetching[key] = true
	r.catalogMu.Unlock()

	go func() {
		cols, cov := fetchColumns(r.mgr, targets, conc, ref.Schema, ref.Name)
		r.catalogMu.Lock()
		// Меняем состояние только если контекст не менялся. При смене контекста
		// colFetching сбрасывается (resetCatalog), и ключ может быть уже выставлен
		// загрузкой НОВОЙ эпохи — безусловный delete сбросил бы этот живой флаг и
		// допустил дублирующую загрузку.
		if r.catalog == cat {
			cat.SetColumns(ref.Schema, ref.Name, cols)
			mergeCoverage(cat, cov)
			delete(r.colFetching, key)
		}
		r.catalogMu.Unlock()
	}()
}

// mergeCoverage добавляет счётчики покрытия колонок в cat.Coverage. Вызывающий
// держит catalogMu.
func mergeCoverage(cat *complete.Catalog, cov map[string]int) {
	if len(cov) == 0 {
		return
	}
	if cat.Coverage == nil {
		cat.Coverage = map[string]int{}
	}
	for k, n := range cov {
		cat.Coverage[k] = n
	}
}

// catSummary — снимок состояния загрузчика каталога (простые значения) для
// "\completion status". Вычисляется под catalogMu, чтобы вывод не читал живые
// поля *Catalog, пока их меняет фоновый загрузчик.
type catSummary struct {
	ready                                  bool
	loading                                bool
	err                                    error
	relations, columns, functions, schemas int
	extensions, enums                      int
	searchPath                             []string // приватная копия
	shards                                 int
	coverage                               bool
	segments                               map[string]complete.LoadState // копия состояний сегментов
}

// catalogStatus снимает состояние загрузчика для \completion status под блокировкой.
func (r *REPL) catalogStatus() catSummary {
	r.catalogMu.Lock()
	defer r.catalogMu.Unlock()
	s := catSummary{loading: r.catalogLoading, err: r.catalogErr}
	if c := r.catalog; c != nil {
		s.ready = true
		s.relations, s.columns = len(c.Relations), len(c.Columns)
		s.functions, s.schemas = len(c.Functions), len(c.Schemas)
		s.extensions, s.enums = len(c.Extensions), len(c.Enums)
		s.searchPath = append([]string(nil), c.SearchPath...)
		s.shards, s.coverage = c.Shards, c.Coverage != nil
		if len(c.Segments) > 0 {
			s.segments = make(map[string]complete.LoadState, len(c.Segments))
			for k, v := range c.Segments {
				s.segments[k] = v
			}
		}
	}
	return s
}

// segmentIssues возвращает отсортированный список проблемных сегментов каталога
// (Status != loaded) в виде «name: status (reason)», чтобы \completion status мог
// показать, что именно недоступно (functions: forbidden (permission denied)).
func segmentIssues(segs map[string]complete.LoadState) []string {
	var out []string
	for name, st := range segs {
		if st.Status == complete.StatusLoaded || st.Status == complete.StatusPending {
			continue
		}
		line := name + ": " + st.Status.String()
		if st.ShardsN > 0 && st.Status == complete.StatusPartial {
			line += fmt.Sprintf(" [%d/%d]", st.ShardsOK, st.ShardsN)
		}
		if st.Error != "" {
			line += " (" + st.Error + ")"
		}
		out = append(out, line)
	}
	sort.Strings(out)
	return out
}

// degradedNotice строит компактную ОДНОСТРОЧНУЮ заметку о деградации каталога
// дополнения (P2-5): какие сегменты недоступны и почему, чтобы пользователь узнал
// об этом проактивно (после фоновой загрузки), не запуская \completion status. ""
// — если все сегменты загружены. Сегменты перечисляются как «name (status)»;
// причина (permission denied/…) добавляется к первому для контекста.
func degradedNotice(segs map[string]complete.LoadState) string {
	type seg struct{ name, status, reason string }
	var bad []seg
	for name, st := range segs {
		if st.Status == complete.StatusLoaded || st.Status == complete.StatusPending {
			continue
		}
		bad = append(bad, seg{name, st.Status.String(), st.Error})
	}
	if len(bad) == 0 {
		return ""
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i].name < bad[j].name })
	parts := make([]string, len(bad))
	for i, b := range bad {
		parts[i] = b.name + " " + b.status
	}
	reason := ""
	for _, b := range bad {
		if b.reason != "" {
			reason = " (" + b.reason + ")"
			break
		}
	}
	return "completion is degraded: " + strings.Join(parts, ", ") + reason + " — \\completion status for details"
}

// takeCatalogNotice возвращает и очищает отложенную заметку о деградации каталога
// (выставляется фоновым загрузчиком). Под catalogMu. Печатается один раз в основном
// цикле перед приглашением.
func (r *REPL) takeCatalogNotice() string {
	r.catalogMu.Lock()
	defer r.catalogMu.Unlock()
	n := r.catalogNotice
	r.catalogNotice = ""
	return n
}

// doCompletion реализует "\completion [status|reload]": показывает состояние
// каталога автодополнения или принудительно перезагружает его.
func (r *REPL) doCompletion(args []string) {
	// Неизвестная подкоманда отвергается, а не молча переходит к выводу статуса
	// (опечатка вроде `\completion relaod` сообщается).
	if len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "reload":
			if len(args) > 1 {
				fmt.Fprintln(r.out, "usage: \\completion reload")
				return
			}
			r.resetCatalog()
			r.kickCatalog()
			fmt.Fprintln(r.out, "completion: catalog reload started in the background")
			return
		case "system":
			// \completion system [on|off] — переключает подсказки объектов
			// pg_catalog/information_schema. По умолчанию off — только своя БД.
			if len(args) > 1 {
				on, err := parseOnOff(args[1:], r.showSystemCatalog)
				if err != nil {
					fmt.Fprintf(r.out, "completion system: %v\n", err)
					return
				}
				r.showSystemCatalog = on
			} else {
				r.showSystemCatalog = !r.showSystemCatalog
			}
			fmt.Fprintf(r.out, "completion: system catalog objects %s\n", onOff(r.showSystemCatalog))
			return
		case "status":
			if len(args) > 1 {
				fmt.Fprintln(r.out, "usage: \\completion status")
				return
			}
			// переход к выводу статуса ниже
		default:
			fmt.Fprintf(r.out, "unknown \\completion subcommand %q (try: status, reload, system [on|off])\n", args[0])
			return
		}
	}
	s := r.catalogStatus()
	switch {
	case s.ready:
		issues := segmentIssues(s.segments)
		state := "ready"
		if len(issues) > 0 {
			state = "ready (partial)"
		}
		fmt.Fprintf(r.out, "completion: %s — %d relations, %d columns, %d functions, %d schemas, %d extensions, %d enums (search_path %s)\n",
			state, s.relations, s.columns, s.functions, s.schemas, s.extensions, s.enums, strings.Join(s.searchPath, ", "))
		for _, p := range issues {
			fmt.Fprintf(r.out, "  %s\n", p)
		}
		if s.shards > 1 {
			covered := "no"
			if s.coverage {
				covered = "yes"
			}
			fmt.Fprintf(r.out, "  shards=%d, per-object coverage probed: %s\n", s.shards, covered)
		}
	case s.loading:
		fmt.Fprintln(r.out, "completion: loading in the background…")
	case s.err != nil:
		fmt.Fprintf(r.out, "completion: last load failed — %v (retries automatically)\n", s.err)
		fmt.Fprintln(r.out, "  use \\completion reload to retry now")
	default:
		fmt.Fprintln(r.out, "completion: not loaded yet (press Tab or run a query)")
	}
}

// buildCatalog строит снимок типизированного каталога (схемы/отношения/колонки/
// функции, search_path, зарезервированные ключевые слова, покрытие по шардам) из
// БД. Не зависит от состояния REPL (безопасно вызывать из горутины): трогает лишь
// общий менеджер пулов под мьютексом. Возвращает nil + ошибку при сбое основного
// запроса (недоступность/таймаут/права).
func buildCatalog(mgr *db.Manager, targets []cluster.Shard, conc int) (*complete.Catalog, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("no shards selected")
	}
	// segs хранит состояние загрузки каждого сегмента (P2-5): ошибка сегмента
	// больше не сворачивается молча в пустой список, а записывается с причиной
	// (forbidden/timeout/failed), чтобы \completion status показал «partial».
	segs := map[string]complete.LoadState{}
	qseg := func(name, sql string) [][]any {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		res, err := mgr.Exec(ctx, targets[0], sql, true)
		segs[name] = segState(err)
		if err != nil || res == nil {
			return nil
		}
		return res.Rows
	}

	cat := &complete.Catalog{Shards: len(targets), Reserved: map[string]bool{}}

	// Отношения с идентичностью схемы. pg_class на живом сервере не бывает пустым,
	// поэтому пустой результат означает сбой запроса.
	rels := qseg("relations", `SELECT n.nspname, c.relname, c.relkind::text
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = ANY('{r,v,m,p,i,S,f}') ORDER BY 1, 2`)
	if len(rels) == 0 {
		return nil, fmt.Errorf("catalog query failed (unreachable, timeout, or insufficient privileges)")
	}
	// relSeen ключует отношение по schema+name, чтобы проход покрытия по шардам мог
	// добавить отношения, существующие только на не-первом шарде, не дублируя уже
	// загруженные из targets[0].
	relSeen := make(map[string]bool, len(rels))
	for _, row := range rels {
		if len(row) < 3 {
			continue
		}
		schema, name := str(row[0]), str(row[1])
		relSeen[schema+"\x00"+name] = true
		cat.Relations = append(cat.Relations, complete.Relation{
			Schema: schema, Name: name, Kind: str(row[2])})
	}
	for _, row := range qseg("schemas", `SELECT nspname FROM pg_namespace ORDER BY 1`) {
		cat.Schemas = append(cat.Schemas, str(row[0]))
	}
	for _, row := range qseg("search_path", `SELECT s FROM unnest(current_schemas(true)) s`) {
		cat.SearchPath = append(cat.SearchPath, str(row[0]))
	}
	// Ключевые слова и те, что требуют кавычек (категории reserved / type_func_name).
	// catcode имеет тип "char"; приводим к text, чтобы читалось как 'R'/'T', а не код ASCII.
	for _, row := range qseg("keywords", `SELECT word, catcode::text FROM pg_get_keywords()`) {
		w := str(row[0])
		cat.Keywords = append(cat.Keywords, w)
		if cc := str(row[1]); cc == "R" || cc == "T" {
			cat.Reserved[w] = true
		}
	}
	// Расширения и enum-типы — каталог-широкие и дешёвые. Их состояние тоже
	// отслеживается в segs (P2-5): теперь \completion status и проактивное
	// уведомление сообщают, если эти сегменты forbidden/timeout, а не молчат.
	for _, row := range qseg("extensions", `SELECT extname FROM pg_extension ORDER BY 1`) {
		cat.Extensions = append(cat.Extensions, str(row[0]))
	}
	for _, row := range qseg("enums", `SELECT n.nspname, t.typname FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE t.typtype = 'e' ORDER BY 1, 2`) {
		if len(row) < 2 {
			continue
		}
		cat.Enums = append(cat.Enums, str(row[0])+"."+str(row[1]))
	}

	// Колонки загружаются ЛЕНИВО для каждого отношения (см. fetchColumns), не
	// заранее — поэтому первый Tab на БД с десятками тысяч колонок дёшев.
	//
	// Функции: по одной представительной (с минимумом аргументов) сигнатуре на
	// schema.name, так что перегрузки в разных схемах НЕ сливаются.
	// Только реально вызываемые в запросе функции: обычные, агрегатные и оконные
	// (prokind f/a/w — НЕ процедуры, требующие CALL). Пропускаем служебное:
	// функции ввода/вывода типов и поддержки (аргумент или возврат internal/cstring)
	// и разные заглушки-обработчики. Так остаются now()/count()/..., а тысячи
	// записей вида int4in отбрасываются.
	for _, row := range qseg("functions", `SELECT DISTINCT ON (n.nspname, p.proname)
		n.nspname, p.proname, pg_get_function_arguments(p.oid), (p.pronargs - p.pronargdefaults)::int, p.prokind::text, p.proretset
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE p.prokind IN ('f','a','w')
		  AND p.prorettype <> ALL (ARRAY['internal','cstring','trigger','event_trigger',
		      'language_handler','fdw_handler','index_am_handler','tsm_handler','table_am_handler']::regtype[])
		  AND NOT (p.proargtypes::oid[] && ARRAY['internal','cstring']::regtype[]::oid[])
		  AND NOT EXISTS (SELECT 1 FROM pg_operator o WHERE p.oid IN (o.oprcode, o.oprrest, o.oprjoin))
		  AND NOT EXISTS (SELECT 1 FROM pg_cast ca WHERE ca.castfunc = p.oid)
		  AND NOT EXISTS (SELECT 1 FROM pg_aggregate a WHERE p.oid IN
		      (a.aggtransfn, a.aggfinalfn, a.aggcombinefn, a.aggserialfn, a.aggdeserialfn,
		       a.aggmtransfn, a.aggminvtransfn, a.aggmfinalfn))
		  AND NOT EXISTS (SELECT 1 FROM pg_type t WHERE p.oid IN
		      (t.typsend, t.typreceive, t.typinput, t.typoutput, t.typmodin, t.typmodout, t.typanalyze))
		ORDER BY n.nspname, p.proname, p.pronargs`) {
		if len(row) < 6 {
			continue
		}
		name := str(row[1])
		cat.Functions = append(cat.Functions, complete.Function{
			Schema: str(row[0]), Name: name, Signature: "(" + str(row[2]) + ")",
			MinArgs: int(asInt64(row[3])), Kind: str(row[4]), NoParen: noParenFuncs[name],
			RetSet: str(row[5]) == "true"})
	}

	// Несколько шардов: заранее замеряем покрытие по ОТНОШЕНИЯМ (один дешёвый запрос
	// на шард), чтобы был виден дрейф таблиц, И объединяем отношения, существующие
	// только на не-первом шарде, чтобы автодополнение предлагало их с покрытием [n/m] —
	// иначе каталог (построенный из targets[0]) их бы не увидел. Покрытие по КОЛОНКАМ
	// считается лениво при загрузке колонок отношения (см. fetchColumns). Схемы/функции/
	// ключевые слова одинаковы по кластеру, поэтому грузятся один раз из targets[0].
	if len(targets) > 1 {
		cat.Coverage = map[string]int{}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		coverageOK := 0
		for _, sr := range mgr.Fanout(ctx, targets,
			`SELECT n.nspname, c.relname, c.relkind::text FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind = ANY('{r,v,m,p,f}')`,
			true, conc, 8*time.Second) {
			if sr.Err != nil || sr.Result == nil {
				continue
			}
			coverageOK++
			for _, row := range sr.Result.Rows {
				if len(row) < 2 || row[0] == nil || row[1] == nil {
					continue
				}
				schema, name := str(row[0]), str(row[1])
				cat.Coverage["rel:"+schema+"."+name]++
				if key := schema + "\x00" + name; !relSeen[key] {
					relSeen[key] = true
					kind := "r"
					if len(row) > 2 && row[2] != nil {
						kind = str(row[2])
					}
					cat.Relations = append(cat.Relations, complete.Relation{
						Schema: schema, Name: name, Kind: kind})
				}
			}
		}
		// Отношения, найденные на поздних шардах, добавлены вне порядка; держим
		// список автодополнения стабильным (по схеме, затем по имени).
		sort.Slice(cat.Relations, func(i, j int) bool {
			if cat.Relations[i].Schema != cat.Relations[j].Schema {
				return cat.Relations[i].Schema < cat.Relations[j].Schema
			}
			return cat.Relations[i].Name < cat.Relations[j].Name
		})
		segs["coverage"] = coverageSegState(coverageOK, len(targets))
	}
	cat.Segments = segs
	return cat, nil
}

// segState классифицирует ошибку запроса сегмента каталога в LoadState
// (forbidden при SQLSTATE 42501, timeout при дедлайне/57014, иначе failed),
// переиспользуя db.ClassifyError. Пустая ошибка -> loaded.
func segState(err error) complete.LoadState {
	st := complete.LoadState{LoadedAt: time.Now()}
	if err == nil {
		st.Status = complete.StatusLoaded
		return st
	}
	info := db.ClassifyError(err)
	st.Error = oneLine(info.Message)
	switch {
	case info.SQLState == "42501":
		st.Status, st.Error = complete.StatusForbidden, "permission denied"
	case info.Kind == "timeout" || info.SQLState == "57014":
		st.Status = complete.StatusTimeout
	default:
		st.Status = complete.StatusFailed
	}
	return st
}

// coverageSegState строит состояние сегмента покрытия по числу ответивших шардов:
// loaded (все), partial (часть) или failed (ни одного).
func coverageSegState(ok, total int) complete.LoadState {
	st := complete.LoadState{LoadedAt: time.Now(), ShardsOK: ok, ShardsN: total}
	switch {
	case ok == 0:
		st.Status = complete.StatusFailed
		st.Error = "no shard answered the coverage probe"
	case ok < total:
		st.Status = complete.StatusPartial
	default:
		st.Status = complete.StatusLoaded
	}
	return st
}

// fetchColumns загружает колонки одного отношения (ленивая загрузка по отношению).
// На одном шарде запрашивает первую цель; на нескольких — разветвляется, чтобы
// заодно посчитать покрытие по колонкам. Возвращает колонки и счётчики покрытия с
// ключом "col:schema.relation.column".
func fetchColumns(mgr *db.Manager, targets []cluster.Shard, conc int, schema, relation string) ([]complete.Column, map[string]int) {
	if len(targets) == 0 {
		return nil, nil
	}
	sql := `SELECT column_name, data_type FROM information_schema.columns
		WHERE table_schema = ` + sqlLiteral(schema) + ` AND table_name = ` + sqlLiteral(relation) + `
		ORDER BY ordinal_position`

	if len(targets) == 1 {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		res, err := mgr.Exec(ctx, targets[0], sql, true)
		if err != nil || res == nil {
			return nil, nil
		}
		var cols []complete.Column
		for _, row := range res.Rows {
			if len(row) >= 2 && row[0] != nil {
				cols = append(cols, complete.Column{Schema: schema, Relation: relation, Name: str(row[0]), Type: str(row[1])})
			}
		}
		return cols, nil
	}

	// Несколько шардов: типы — с первого ответившего шарда, покрытие — со всех.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cov := map[string]int{}
	typeOf := map[string]string{}
	var order []string
	for _, sr := range mgr.Fanout(ctx, targets, sql, true, conc, 8*time.Second) {
		if sr.Err != nil || sr.Result == nil {
			continue
		}
		for _, row := range sr.Result.Rows {
			if len(row) < 2 || row[0] == nil {
				continue
			}
			name := str(row[0])
			cov["col:"+schema+"."+relation+"."+name]++
			if _, seen := typeOf[name]; !seen {
				typeOf[name] = str(row[1])
				order = append(order, name)
			}
		}
	}
	cols := make([]complete.Column, 0, len(order))
	for _, name := range order {
		cols = append(cols, complete.Column{Schema: schema, Relation: relation, Name: name, Type: typeOf[name]})
	}
	return cols, cov
}

// fetchPrimaryKey возвращает имена колонок первичного ключа (schema, relation),
// запрашивая на одной цели через разрешение ::regclass. Пусто, если у таблицы нет
// первичного ключа или при ошибке.
func fetchPrimaryKey(mgr *db.Manager, target cluster.Shard, schema, relation string) []string {
	ref := complete.QuoteIdent(relation, nil)
	if schema != "" {
		ref = complete.QuoteIdent(schema, nil) + "." + ref
	}
	sql := `SELECT a.attname FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		WHERE i.indrelid = ` + sqlLiteral(ref) + `::regclass AND i.indisprimary
		ORDER BY array_position(i.indkey, a.attnum)`
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	res, err := mgr.Exec(ctx, target, sql, true)
	if err != nil || res == nil {
		return nil
	}
	var out []string
	for _, row := range res.Rows {
		if len(row) > 0 && row[0] != nil {
			out = append(out, str(row[0]))
		}
	}
	return out
}

// pkColumns возвращает закэшированные колонки первичного ключа (schema, relation),
// запуская фоновую загрузку при промахе кэша. Не блокирует — используется в
// автодополнении, результат появляется на следующей отрисовке. ready сообщает о
// попадании в кэш.
func (r *REPL) pkColumns(schema, relation string) (cols []string, ready bool) {
	key := schema + "\x00" + relation
	r.catalogMu.Lock()
	if c, ok := r.pkCache[key]; ok {
		r.catalogMu.Unlock()
		return c, true
	}
	if r.pkFetching[key] || len(r.targets) == 0 {
		r.catalogMu.Unlock()
		return nil, false
	}
	if r.pkFetching == nil {
		r.pkFetching = map[string]bool{}
	}
	r.pkFetching[key] = true
	mgr, target, epoch := r.mgr, r.targets[0], r.catalogEpoch
	r.catalogMu.Unlock()

	go func() {
		pk := fetchPrimaryKey(mgr, target, schema, relation)
		r.catalogMu.Lock()
		if epoch == r.catalogEpoch { // контекст не менялся — принимаем результат
			if r.pkCache == nil {
				r.pkCache = map[string][]string{}
			}
			r.pkCache[key] = pk
			delete(r.pkFetching, key)
		}
		r.catalogMu.Unlock()
	}()
	return nil, false
}

// primaryKeyColumns возвращает колонки первичного ключа (schema, relation),
// блокируясь при промахе кэша (путь явных команд \find/\count).
func (r *REPL) primaryKeyColumns(schema, relation string) []string {
	key := schema + "\x00" + relation
	r.catalogMu.Lock()
	if c, ok := r.pkCache[key]; ok {
		r.catalogMu.Unlock()
		return c
	}
	r.catalogMu.Unlock()
	if len(r.targets) == 0 {
		return nil
	}
	pk := fetchPrimaryKey(r.mgr, r.targets[0], schema, relation)
	r.catalogMu.Lock()
	if r.pkCache == nil {
		r.pkCache = map[string][]string{}
	}
	r.pkCache[key] = pk
	r.catalogMu.Unlock()
	return pk
}

// pkeyShorthand — разобранный аргумент поиска по ключу "[schema.]table.column<op>value".
type pkeyShorthand struct {
	schema, table, col string // идентичность без кавычек для поиска PK
	tableTok           string // безопасно закавыченная [schema.]table для запроса
	op, val            string // оператор сравнения и его сырое правое значение
	cond               string // закавыченное WHERE-выражение "column<op>value"
}

var (
	simpleNumRe = regexp.MustCompile(`^[+-]?\d+(\.\d+)?$`)
	simpleStrRe = regexp.MustCompile(`^'([^']|'')*'$`) // в одинарных кавычках, '' экранирует
)

// isSimpleKeyValue сообщает, является ли v одиночным литералом, безопасным для
// шортката поиска по ключу: число, строка в кавычках или true/false/null. Всё с
// операторами, ключевыми словами, скобками или вторым токеном (and/or, UNION,
// подзапрос) отвергается — шорткат выражает только один поиск по ключу.
func isSimpleKeyValue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	if simpleNumRe.MatchString(v) || simpleStrRe.MatchString(v) {
		return true
	}
	switch strings.ToLower(v) {
	case "true", "false", "null":
		return true
	}
	return false
}

// parsePkeyShorthand распознаёт компактную форму "[schema.]table.column<op>value"
// (напр. items.items.item_id=100), используемую \find/\count, разбивая её на
// таблицу, ключевую колонку и условие WHERE. ok=false для обычной формы
// "table [where ...]" с пробелами.
func parsePkeyShorthand(args []string) (pkeyShorthand, bool) {
	var sh pkeyShorthand
	if len(args) == 0 {
		return sh, false
	}
	joined := strings.TrimSpace(strings.Join(args, " "))
	opIdx := strings.IndexAny(joined, "=<>!~")
	if opIdx <= 0 {
		return sh, false
	}
	parts, err := splitQualified(strings.TrimSpace(joined[:opIdx]))
	if err != nil || len(parts) < 2 {
		return sh, false
	}
	// Оператор занимает байты сравнения; всё после — значение.
	opEnd := opIdx
	for opEnd < len(joined) && strings.IndexByte("=<>!~", joined[opEnd]) >= 0 {
		opEnd++
	}
	sh.op = joined[opIdx:opEnd]
	sh.val = strings.TrimSpace(joined[opEnd:])
	sh.col = parts[len(parts)-1]
	tp := parts[:len(parts)-1]
	switch len(tp) {
	case 1:
		sh.table = tp[0]
		sh.tableTok = complete.QuoteIdent(tp[0], nil)
	case 2:
		sh.schema, sh.table = tp[0], tp[1]
		sh.tableTok = complete.QuoteIdent(tp[0], nil) + "." + complete.QuoteIdent(tp[1], nil)
	default:
		return sh, false // таблица — максимум schema.name
	}
	sh.cond = complete.QuoteIdent(sh.col, nil) + sh.op + sh.val
	return sh, true
}

// countArgs приводит аргументы \count/\locate к [table] или [table, condition].
// Разворачивает шорткат table.pkey=value (синтезированное условие, без риска
// пробелов), а для обычной формы восстанавливает WHERE из СЫРОЙ строки, чтобы
// сохранить точные пробелы строкового литерала — strings.Fields + склейка одним
// пробелом схлопнули бы 'a  b' в 'a b' и тихо сматчили другие строки. raw — полная
// строка мета-команды; rawTail(raw, 2) отбрасывает "\count"/"\locate" и токен
// таблицы, оставляя WHERE как есть.
func (r *REPL) countArgs(args []string, raw string) ([]string, error) {
	_, shorthand := parsePkeyShorthand(args)
	resolved, err := r.resolvePkeyShorthand(args)
	if err != nil {
		return nil, err
	}
	if shorthand {
		return resolved, nil // [table, cond] уже точны
	}
	out := []string{resolved[0]}
	if where := strings.TrimSpace(rawTail(raw, 2)); where != "" {
		out = append(out, where)
	}
	return out, nil
}

// resolvePkeyShorthand переписывает шорткат "table.pkey=value" в обычные аргументы
// [table, condition], требуя, чтобы ключевая колонка была первичным ключом таблицы.
// Обычные аргументы проходят без изменений.
func (r *REPL) resolvePkeyShorthand(args []string) ([]string, error) {
	sh, ok := parsePkeyShorthand(args)
	if !ok {
		// Одиночный токен из 3 частей через точку (items.items.item_id) — недопечатанный
		// шорткат ключа без значения; подсказываем вместо общей ошибки таблицы.
		if len(args) == 1 {
			if parts, err := splitQualified(args[0]); err == nil && len(parts) >= 3 {
				return nil, fmt.Errorf("для поиска по ключу укажите значение: %s.<pkey>=<значение>",
					strings.Join(parts[:len(parts)-1], "."))
			}
		}
		return args, nil
	}
	// Ключевая колонка должна быть первичным ключом (проверяется первой — самое
	// полезное сообщение называет реальный pkey).
	pk := r.primaryKeyColumns(sh.schema, sh.table)
	if len(pk) == 0 {
		return nil, fmt.Errorf("у таблицы %s нет первичного ключа (или она недоступна) — поиск по ключу невозможен", sh.tableTok)
	}
	if !containsFold(pk, sh.col) {
		return nil, fmt.Errorf("искать можно только по первичному ключу %s (pkey: %s), а указан %q",
			sh.tableTok, strings.Join(pk, ", "), sh.col)
	}
	// Шорткат выражает ровно один поиск по ключу: значение должно быть одиночным
	// литералом. Всё иное (and/or, подзапрос, UNION, второй предикат) отвергается —
	// это закрывает значение как канал инъекции/эксфильтрации и указывает на
	// свободную форму для действительно составных условий.
	if !isSimpleKeyValue(sh.val) {
		return nil, fmt.Errorf("шорткат по ключу принимает одно простое значение (число, 'строка', true/false/null), а не выражение; для составных условий: \\find %s <where>",
			sh.tableTok)
	}
	return []string{sh.tableTok, sh.cond}, nil
}

// containsFold сообщает, содержит ли ss строку s без учёта регистра.
func containsFold(ss []string, s string) bool {
	for _, x := range ss {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// serverVersion возвращает server_version_num текущего хранилища (кэшируется; 0
// если неизвестно), чтобы анализатор плана мог гейтить правила по версии.
func (r *REPL) serverVersion() int {
	// Поле serverVer защищено catalogMu (см. repl.go и resetCatalog). Читаем кэш
	// под локом: иначе параллельные горутины (например, фан-аут doctorAll) гоняют
	// чтение/запись r.serverVer, если прогрев вернул 0.
	r.catalogMu.Lock()
	if r.serverVer != 0 || len(r.targets) == 0 {
		v := r.serverVer
		r.catalogMu.Unlock()
		return v
	}
	target := r.targets[0]
	r.catalogMu.Unlock()
	// Сетевой запрос выполняем без лока (catalogMu охраняет состояние в памяти, не
	// блокирующий I/O), затем публикуем результат под локом.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ver := 0
	if res, err := r.mgr.Exec(ctx, target, `SELECT current_setting('server_version_num')::int`, true); err == nil && res != nil && len(res.Rows) > 0 && len(res.Rows[0]) > 0 {
		ver = int(asInt64(res.Rows[0][0]))
	}
	r.catalogMu.Lock()
	defer r.catalogMu.Unlock()
	// Другая горутина могла уже заполнить кэш — не перетираем его нулём.
	if r.serverVer == 0 && ver != 0 {
		r.serverVer = ver
	}
	return r.serverVer
}

// noParenFuncs — особые нульарные SQL-функции, вызываемые БЕЗ скобок (current_user,
// а не current_user()). Добавление "()" к ним даёт некорректный SQL.
var noParenFuncs = map[string]bool{
	"current_catalog": true, "current_date": true, "current_role": true,
	"current_schema": true, "current_time": true, "current_timestamp": true,
	"current_user": true, "localtime": true, "localtimestamp": true,
	"session_user": true, "user": true, "system_user": true,
}

// funcToken отдаёт имя функции как токен для вставки при автодополнении: голое для
// нульарных SQL-специальных, "name()" для функций без аргументов, иначе "name(".
func funcToken(name string, minArgs int64) string {
	if noParenFuncs[name] {
		return name
	}
	if minArgs == 0 {
		return name + "()"
	}
	return name + "("
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int16: // smallint (напр. pg_proc.pronargs) — pgx возвращает int16
		return int64(n)
	case int:
		return int64(n)
	default:
		return 0
	}
}

// asFloat64 приводит числовую ячейку pgx к float64 (pg_stats.n_distinct — real →
// float32; целые — через asInt64). 0 для нечисловых/NULL.
func asFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int64, int32, int16, int:
		return float64(asInt64(v))
	default:
		return 0
	}
}

// stripLeadingWhere убирает необязательное ведущее "where " из условия.
func stripLeadingWhere(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 6 && strings.EqualFold(s[:6], "where ") {
		return strings.TrimSpace(s[6:])
	}
	return s
}

var plainIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

// splitQualified разбирает необязательно квалифицированный схемой идентификатор на
// голые (без кавычек) части, принимая простые и двукавыченные идентификаторы
// ("My Table") и отвергая всё прочее (пробелы, скобки, точки с запятой, операторы),
// чтобы аргумент таблицы \count/\locate не мог инъектировать SQL.
func splitQualified(tok string) ([]string, error) {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return nil, fmt.Errorf("empty table name")
	}
	var parts []string
	i := 0
	for i < len(tok) {
		if tok[i] == '"' {
			j := i + 1
			var sb strings.Builder
			for j < len(tok) {
				if tok[j] == '"' {
					if j+1 < len(tok) && tok[j+1] == '"' { // "" -> литеральная кавычка
						sb.WriteByte('"')
						j += 2
						continue
					}
					break
				}
				sb.WriteByte(tok[j])
				j++
			}
			if j >= len(tok) || tok[j] != '"' {
				return nil, fmt.Errorf("unterminated quoted identifier in %q", tok)
			}
			if sb.Len() == 0 {
				return nil, fmt.Errorf("empty quoted identifier in %q", tok)
			}
			parts = append(parts, sb.String())
			i = j + 1
		} else {
			j := i
			for j < len(tok) && tok[j] != '.' {
				j++
			}
			seg := tok[i:j]
			if !plainIdentRe.MatchString(seg) {
				return nil, fmt.Errorf("invalid identifier %q (use a plain or \"quoted\" name)", seg)
			}
			parts = append(parts, seg)
			i = j
		}
		if i < len(tok) { // ожидаем разделитель-точку
			if tok[i] != '.' {
				return nil, fmt.Errorf("invalid table name %q", tok)
			}
			i++
			if i >= len(tok) {
				return nil, fmt.Errorf("trailing '.' in %q", tok)
			}
		}
	}
	return parts, nil
}

// parseTableArg проверяет необязательно квалифицированное схемой имя таблицы и
// возвращает его безопасно закавыченным для использования в SQL.
func parseTableArg(tok string) (string, error) {
	parts, err := splitQualified(tok)
	if err != nil {
		return "", err
	}
	switch len(parts) {
	case 1:
		return complete.QuoteIdent(parts[0], nil), nil
	case 2:
		return complete.QuoteIdent(parts[0], nil) + "." + complete.QuoteIdent(parts[1], nil), nil
	default:
		return "", fmt.Errorf("table must be [schema.]name, got %q", tok)
	}
}

// buildCountQuery строит безопасный `SELECT count(*) FROM <table> [WHERE <cond>]`
// из аргументов \count/\locate: таблица строго разбирается и закавычивается, а
// условие не может содержать разделитель операторов (чтобы нельзя было прицепить
// второй оператор в обход read-only-гейта).
func (r *REPL) buildCountQuery(args []string) (string, error) {
	table, err := parseTableArg(args[0])
	if err != nil {
		return "", err
	}
	q := "SELECT count(*) AS count FROM " + table
	if where := stripLeadingWhere(strings.Join(args[1:], " ")); where != "" {
		if strings.Contains(sqlsplit.Mask(where), ";") {
			return "", fmt.Errorf("condition must be a single boolean expression (no ';')")
		}
		q += " WHERE " + where
	}
	return q, nil
}

// doCount выполняет count(*) на каждой цели и показывает счётчики по шардам плюс
// общий итог по кластеру — то, чего psql не умеет между базами.
func (r *REPL) doCount(args []string, raw string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: \\count <table> [where ...]")
	}
	args, err := r.countArgs(args, raw)
	if err != nil {
		return err
	}
	q, err := r.buildCountQuery(args)
	if err != nil {
		return err
	}
	results := r.fanoutRead(q)

	var rows [][]string
	var total int64
	fails := 0
	for _, sr := range results {
		if sr.Err != nil {
			fails++
			rows = append(rows, []string{sr.Shard.LabelDB(), "ERR: " + oneLine(sr.Err.Error())})
			continue
		}
		var cnt int64
		if sr.Result != nil && len(sr.Result.Rows) > 0 && len(sr.Result.Rows[0]) > 0 {
			cnt = asInt64(sr.Result.Rows[0][0])
		}
		total += cnt
		rows = append(rows, []string{sr.Shard.LabelDB(), fmt.Sprintf("%d", cnt)})
	}
	footer := fmt.Sprintf("TOTAL: %d across %d shard(s)", total, len(results)-fails)
	if fails > 0 {
		footer += fmt.Sprintf(" (%d failed)", fails)
	}
	render.Table(r.out, []string{"shard", "count"}, rows, footer)
	return nil
}

// doLocate находит шарды, где есть строки под условие, и при ровно одном
// совпадении сразу переключается на него. Это ответ на "не знаю, на каком шарде
// лежит этот itemId".
func (r *REPL) doLocate(args []string, raw string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: \\locate <table> [where ...]")
	}
	args, err := r.countArgs(args, raw)
	if err != nil {
		return err
	}
	q, err := r.buildCountQuery(args)
	if err != nil {
		return err
	}
	results := r.fanoutRead(q)

	type hit struct {
		label string
		cnt   int64
	}
	var hits []hit
	fails := 0
	for _, sr := range results {
		if sr.Err != nil {
			fails++
			continue
		}
		var cnt int64
		if sr.Result != nil && len(sr.Result.Rows) > 0 && len(sr.Result.Rows[0]) > 0 {
			cnt = asInt64(sr.Result.Rows[0][0])
		}
		if cnt > 0 {
			hits = append(hits, hit{sr.Shard.Label, cnt})
		}
	}

	if len(hits) == 0 {
		fmt.Fprintf(r.out, "no matching rows on any of %d shard(s)", len(results))
		if fails > 0 {
			fmt.Fprintf(r.out, " (%d failed)", fails)
		}
		fmt.Fprintln(r.out)
		return nil
	}

	rows := make([][]string, 0, len(hits))
	for _, h := range hits {
		rows = append(rows, []string{h.label, fmt.Sprintf("%d", h.cnt)})
	}
	render.Table(r.out, []string{"shard", "count"}, rows,
		fmt.Sprintf("found on %d/%d shard(s)", len(hits), len(results)))

	if len(hits) == 1 {
		// Данные ровно на одном шарде — автоматически переключаемся туда.
		return r.retarget(hits[0].label)
	}
	fmt.Fprintf(r.out, "switch with: \\s <label>\n")
	return nil
}

// doPing сообщает доступность и задержку для каждого целевого шарда.
func (r *REPL) doPing() {
	ctx, cancel := interruptible() // Ctrl-C прерывает медленную/недоступную пробу
	defer cancel()
	results := r.mgr.ForEachShard(ctx, r.targets, r.cfg.ProbeConcurrency(len(r.targets)), time.Duration(r.cfg.QueryTimeout),
		false, // пробуем каждый шард ради статуса — не останавливаемся на первой ошибке
		func(ctx context.Context, s cluster.Shard) (int64, error) {
			_, err := r.mgr.Exec(ctx, s, "select 1", true)
			return 0, err
		})
	rows := make([][]string, 0, len(results))
	ok := 0
	for _, res := range results {
		status := "OK"
		if res.Err != nil {
			status = "FAIL: " + oneLine(res.Err.Error())
		} else {
			ok++
		}
		rows = append(rows, []string{res.Shard.LabelDB(), status, fmt.Sprintf("%dms", res.Duration.Milliseconds())})
	}
	render.Table(r.out, []string{"shard", "status", "latency"}, rows,
		fmt.Sprintf("%d/%d reachable", ok, len(results)))
}

// doDiff сравнивает колонки и индексы таблицы по всем целевым шардам и сообщает о
// дрейфе схемы — напр. о миграции, дошедшей не до всех шардов.
func (r *REPL) doDiff(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: \\diff <table> | <schema>.<table>")
	}
	// Разбираем необязательный квалификатор схемы, чтобы `public.users` сопоставлялся
	// как (schema=public, table=users), а не сравнивался целиком с голым table_name.
	parts, err := splitQualified(args[0])
	if err != nil {
		return fmt.Errorf("\\diff: %v", err)
	}
	var schema, table string
	switch len(parts) {
	case 1:
		table = parts[0]
	case 2:
		schema, table = parts[0], parts[1]
	default:
		return fmt.Errorf("\\diff: expected <table> or <schema>.<table>, got %q", args[0])
	}
	// Разрешаем отношение в ОДИН OID через to_regclass на КАЖДОМ шарде: голое имя
	// сопоставляется через search_path этого шарда (а не агрегируется по всем
	// одноимённым таблицам в любой схеме), а квалифицированное схемой имя — точно.
	// to_regclass возвращает NULL, если отношения нет, поэтому шард без таблицы даёт
	// запасное '<MISSING>', а не ложное совпадение.
	qualified := complete.QuoteIdent(table, nil)
	if schema != "" {
		qualified = complete.QuoteIdent(schema, nil) + "." + complete.QuoteIdent(table, nil)
	}
	regclass := "to_regclass(" + sqlLiteral(qualified) + ")"

	// Сравниваем широкую сигнатуру, чтобы поймать дрейф: коллацию колонок,
	// ограничения (CHECK/FK/UNIQUE/PK), триггеры, вид таблицы и партиционирование.
	colsQ := `SELECT coalesce(string_agg(
		a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
		|| CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE '' END
		|| coalesce(' DEFAULT ' || pg_get_expr(d.adbin, d.adrelid), '')
		|| CASE WHEN a.attidentity <> '' THEN ' IDENTITY' ELSE '' END
		|| CASE WHEN a.attgenerated <> '' THEN ' GENERATED' ELSE '' END
		|| coalesce(' COLLATE ' || (SELECT quote_ident(cl.collname) FROM pg_collation cl
			WHERE cl.oid = a.attcollation AND cl.collname <> 'default'), ''),
		', ' ORDER BY a.attnum), '<MISSING>')
		FROM pg_attribute a
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = ` + regclass + ` AND a.attnum > 0 AND NOT a.attisdropped`
	// Индексы: показываем определение И статус валидности — невалидный/не-готовый
	// индекс (сорванный CREATE INDEX CONCURRENTLY) иначе невидим как дрейф.
	idxQ := `SELECT coalesce(string_agg(
		pg_get_indexdef(i.indexrelid)
		|| CASE WHEN NOT i.indisvalid THEN ' [INVALID]' ELSE '' END
		|| CASE WHEN NOT i.indisready THEN ' [NOT READY]' ELSE '' END,
		' | ' ORDER BY ic.relname), '<none>')
		FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid
		WHERE i.indrelid = ` + regclass
	// pg_get_constraintdef уже включает суффикс NOT VALID для непровалидированных
	// ограничений, поэтому состояние валидации ловится автоматически.
	constrQ := `SELECT coalesce(string_agg(pg_get_constraintdef(oid), ' | ' ORDER BY conname), '<none>')
		FROM pg_constraint WHERE conrelid = ` + regclass + ` AND contype IN ('c','f','u','p')`
	trigQ := `SELECT coalesce(string_agg(tgname || ' ' || pg_get_triggerdef(oid), ' | ' ORDER BY tgname), '<none>')
		FROM pg_trigger WHERE tgrelid = ` + regclass + ` AND NOT tgisinternal`
	// RLS: включена ли row-level security, FORCE и сами политики (cmd/USING/CHECK).
	rlsQ := `SELECT coalesce(
		(SELECT CASE WHEN c.relrowsecurity THEN 'rls' ELSE 'no-rls' END
		   || CASE WHEN c.relforcerowsecurity THEN ' FORCE' ELSE '' END
		 FROM pg_class c WHERE c.oid = ` + regclass + `), '<MISSING>')
		|| coalesce(' | ' || (SELECT string_agg(
			polname || ':' || polcmd::text
			|| coalesce(' USING(' || pg_get_expr(polqual, polrelid) || ')', '')
			|| coalesce(' CHECK(' || pg_get_expr(polwithcheck, polrelid) || ')', ''),
			' | ' ORDER BY polname)
		   FROM pg_policy WHERE polrelid = ` + regclass + `), '')`
	// Определение view/materialized view (для не-view даёт '<not-a-view>').
	defQ := `SELECT coalesce(pg_get_viewdef(` + regclass + `, true), '<not-a-view>')`
	// Метаданные таблицы: вид, партиционирование (ключ) и СОБСТВЕННАЯ граница партиции
	// (relpartbound) — чтобы дочерние партиции с разными границами тоже ловились.
	metaQ := `SELECT coalesce((SELECT c.relkind::text
		|| CASE WHEN c.relispartition THEN ' [partition ' || coalesce(pg_get_expr(c.relpartbound, c.oid), '') || ']' ELSE '' END
		|| coalesce(' ' || pg_get_partkeydef(c.oid), '')
		FROM pg_class c WHERE c.oid = ` + regclass + `), '<MISSING>')`
	// Привилегии таблицы (дрейф GRANT/REVOKE): grantee:priv, отсортировано; PUBLIC для
	// grantee=0. NULL relacl (только дефолтные права владельца) даёт '<none>'.
	grantsQ := `SELECT coalesce(string_agg(
		CASE WHEN g.grantee = 0 THEN 'PUBLIC' ELSE g.grantee::regrole::text END || ':' || g.privilege_type,
		' | ' ORDER BY g.grantee, g.privilege_type), '<none>')
		FROM (SELECT (aclexplode(c.relacl)).* FROM pg_class c WHERE c.oid = ` + regclass + `) g`
	// Расширенная статистика (pg_statistic_ext): дрейф объектов статистики таблицы.
	statsQ := `SELECT coalesce(string_agg(
		stxname || ' (' || array_to_string(stxkind, ',') || ')', ' | ' ORDER BY stxname), '<none>')
		FROM pg_statistic_ext WHERE stxrelid = ` + regclass

	sig := func(results []db.ShardResult) map[string]string {
		m := map[string]string{}
		for _, sr := range results {
			if sr.Err != nil {
				m[sr.Shard.Label] = "ERR: " + oneLine(sr.Err.Error())
				continue
			}
			if sr.Result != nil && len(sr.Result.Rows) > 0 && len(sr.Result.Rows[0]) > 0 && sr.Result.Rows[0][0] != nil {
				m[sr.Shard.Label] = fmt.Sprintf("%v", sr.Result.Rows[0][0])
			}
		}
		return m
	}

	colsResults := r.fanoutRead(colsQ)
	dims := []struct {
		name string
		sig  map[string]string
	}{
		{"table", sig(r.fanoutRead(metaQ))},
		{"columns", sig(colsResults)},
		{"indexes", sig(r.fanoutRead(idxQ))},
		{"constraints", sig(r.fanoutRead(constrQ))},
		{"triggers", sig(r.fanoutRead(trigQ))},
		{"rls", sig(r.fanoutRead(rlsQ))},
		{"grants", sig(r.fanoutRead(grantsQ))},
		{"statistics", sig(r.fanoutRead(statsQ))},
		{"definition", sig(r.fanoutRead(defQ))},
	}

	// Группируем шарды по совокупной сигнатуре всех сравниваемых измерений.
	type variant struct {
		vals   []string
		shards []string
	}
	groups := map[string]*variant{}
	var order []string
	for _, sr := range colsResults {
		label := sr.Shard.Label
		vals := make([]string, len(dims))
		var kb strings.Builder
		for i, dm := range dims {
			vals[i] = dm.sig[label]
			kb.WriteString(vals[i])
			kb.WriteByte(0)
		}
		key := kb.String()
		g, ok := groups[key]
		if !ok {
			g = &variant{vals: vals}
			groups[key] = g
			order = append(order, key)
		}
		g.shards = append(g.shards, label)
	}
	printVariant := func(v *variant) {
		for i, dm := range dims {
			fmt.Fprintf(r.out, "  %-12s %s\n", dm.name+":", v.vals[i])
		}
	}

	if len(groups) <= 1 {
		fmt.Fprintf(r.out, "all %d shard(s) identical for %s\n", len(colsResults), args[0])
		if len(order) == 1 {
			printVariant(groups[order[0]])
		}
		return nil
	}

	fmt.Fprintf(r.out, "DRIFT: %d schema variants for %s across %d shard(s)\n", len(groups), args[0], len(colsResults))
	// Какие именно измерения расходятся — сигнал, на что смотреть в первую очередь
	// (rls/constraints/indexes семантически опаснее триггеров/метаданных).
	var differing []string
	for i, dm := range dims {
		seen := map[string]bool{}
		for _, key := range order {
			seen[groups[key].vals[i]] = true
		}
		if len(seen) > 1 {
			differing = append(differing, dm.name)
		}
	}
	if len(differing) > 0 {
		fmt.Fprintf(r.out, "differing dimensions: %s\n", strings.Join(differing, ", "))
	}
	for i, key := range order {
		g := groups[key]
		sort.Strings(g.shards)
		fmt.Fprintf(r.out, "\nvariant %d — %d shard(s): %s\n", i+1, len(g.shards), strings.Join(g.shards, ", "))
		printVariant(g)
	}
	return nil
}

// sqlLiteral отдаёт строку как SQL-литерал в одинарных кавычках.
func sqlLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// dmlUpdateLeadRe / dmlDeleteLeadRe распознают ведущий глагол UPDATE / DELETE FROM,
// допуская любой пробел после него (в т.ч. '\n'/'\t' при многострочном вводе), а не
// только одиночный пробел.
var (
	dmlUpdateLeadRe = regexp.MustCompile(`^update\s+`)
	dmlDeleteLeadRe = regexp.MustCompile(`^delete\s+from\s+`)
)

// validTarget сообщает, годится ли цель UPDATE/DELETE (таблица с необязательным
// алиасом, напр. "foo", "only foo", "foo AS f") для предпросмотра count(*). Отвергаем
// пустое, скобки/запятые (подзапрос или список таблиц) и незакрытые кавычки — лучше
// пропустить предпросмотр, чем выдать некорректный запрос.
func validTarget(t string) bool {
	return t != "" && !strings.ContainsAny(t, "(),") && strings.Count(t, `"`)%2 == 0
}

// topLevelWhere возвращает предложение WHERE на глубине скобок 0 и вне строковых
// литералов/комментариев (чтобы "where" подзапроса или литерала не приняли за
// нужное) либо "" при отсутствии. Распознаёт WHERE как токен, ограниченный с обеих
// сторон любым не-идентификаторным байтом — в т.ч. пробельным символом, напр.
// переводом строки (многострочный ввод склеен через '\n'), или '(' как в WHERE(...),
// а не только обычным пробелом.
func topLevelWhere(s string) string {
	low := strings.ToLower(sqlsplit.Mask(s)) // литералы/комментарии/идентификаторы нейтрализованы, длина сохранена
	depth := 0
	for i := 0; i < len(low); i++ {
		switch low[i] {
		case '(':
			depth++
			continue
		case ')':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth == 0 && low[i] == 'w' && i+5 <= len(low) && low[i:i+5] == "where" {
			beforeOK := i == 0 || !identByte(low[i-1])
			afterOK := i+5 == len(low) || !identByte(low[i+5])
			if beforeOK && afterOK {
				// Режем ИСХОДНУЮ строку (смещения совпадают: Mask сохраняет длину).
				return strings.TrimSuffix(strings.TrimSpace(s[i+5:]), ";")
			}
		}
	}
	return ""
}

// identByte сообщает, может ли c быть частью SQL-идентификатора (для границ слов).
func identByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// parseDML по возможности извлекает таблицу и верхнеуровневый WHERE из
// UPDATE/DELETE для предпросмотра влияния перед записью. ok=false, если уверенно
// разобрать не удаётся.
func parseDML(sql string) (table, where string, ok bool) {
	s := strings.TrimSpace(sql)
	// Маскируем литералы/комментарии/кавыченные идентификаторы (длина сохранена, так что
	// смещения в masked совпадают с s); FROM/USING/SET/WHERE ищем только на верхнем
	// уровне скобок, чтобы клауза подзапроса не принималась за нужную.
	masked := strings.ToLower(sqlsplit.Mask(s))
	switch {
	case dmlUpdateLeadRe.MatchString(masked):
		// Многотабличный UPDATE ... FROM нельзя превратить в честный предпросмотр
		// `count(*) FROM <table> WHERE ...` (join меняет, какие строки подходят).
		if hasTopLevelClause(masked, "from") {
			return "", "", false
		}
		verbEnd := len(dmlUpdateLeadRe.FindString(masked))
		setPos := topLevelClauseIndex(masked, "set")
		if setPos <= verbEnd {
			return "", "", false // нет верхнеуровневого SET — не разбираем
		}
		// Цель — всё между глаголом и SET: таблица с необязательным алиасом. Сохраняем
		// алиас (напр. "foo AS f"), иначе WHERE с "f.*" не резолвился бы в предпросмотре.
		table = strings.TrimSpace(s[verbEnd:setPos])
	case dmlDeleteLeadRe.MatchString(masked):
		if hasTopLevelClause(masked, "using") {
			return "", "", false
		}
		verbEnd := len(dmlDeleteLeadRe.FindString(masked))
		end := len(s)
		if p := topLevelClauseIndex(masked, "where"); p >= 0 {
			end = p
		} else if p := topLevelClauseIndex(masked, "returning"); p >= 0 {
			end = p
		}
		table = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s[verbEnd:end]), ";"))
	default:
		return "", "", false
	}
	if !validTarget(table) {
		return "", "", false
	}
	return table, topLevelWhere(s), true
}

// hasTopLevelClause сообщает, содержит ли замаскированный SQL в нижнем регистре
// ключевое слово kw как отдельное слово на глубине скобок 0 (так FROM/USING внутри
// подзапроса или вызова функции не учитывается).
// topLevelClauseIndex возвращает байтовый индекс ключевого слова kw как отдельного
// слова на глубине скобок 0 (FROM/USING/SET внутри подзапроса или вызова функции не
// учитывается), или -1, если его нет.
func topLevelClauseIndex(masked, kw string) int {
	depth := 0
	for i := 0; i < len(masked); i++ {
		switch masked[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
		if depth != 0 {
			continue
		}
		if i+len(kw) <= len(masked) && masked[i:i+len(kw)] == kw {
			before := i == 0 || !identByte(masked[i-1])
			after := i+len(kw) == len(masked) || !identByte(masked[i+len(kw)])
			if before && after {
				return i
			}
		}
	}
	return -1
}

// hasTopLevelClause сообщает, есть ли ключевое слово kw как отдельное слово на глубине
// скобок 0.
func hasTopLevelClause(masked, kw string) bool { return topLevelClauseIndex(masked, kw) >= 0 }

// previewImpact перед записью показывает, сколько строк UPDATE/DELETE затронет на
// каждом шарде (по возможности). Возвращает countsKnown=false, если предпросмотр не
// смог получить счётчик с КАЖДОЙ цели (шард с ошибкой показан как "?"), чтобы
// вызывающий мог сработать "fail closed" и потребовать явного подтверждения, а не
// пропустить неизвестный радиус поражения. Оператор, который вовсе нельзя показать в
// предпросмотре (не UPDATE/DELETE или UPDATE..FROM), возвращает true — действовать
// не на что, а обычный гейт записи всё равно применяется.
func (r *REPL) previewImpact(sql string) (countsKnown bool) {
	if !r.impact {
		return true
	}
	table, where, ok := parseDML(sql)
	if !ok {
		return true
	}
	q := "SELECT count(*) AS would_affect FROM " + table
	if where != "" {
		q += " WHERE " + where
	}
	// Серверный statement_timeout для count-предпросмотра, как и для обычных чтений
	// (execReadCtx/diagnose): на большой таблице count перед UPDATE/DELETE должен
	// подчиняться тому же таймауту, что виден в приглашении, а не только клиентскому.
	r.mgr.SetReadTimeout(r.stmtTimeout)
	defer r.mgr.SetReadTimeout("")
	results := r.fanoutRead(q)
	// Нет ни одного результата (нет целей/всё отвалилось до запроса) — радиус
	// поражения неизвестен. Fail closed: требуем явного подтверждения, а не
	// пропускаем запись с молчаливым "всё известно".
	if len(results) == 0 {
		return false
	}
	var rows [][]string
	var total int64
	parsed := false
	hadError := false
	for _, sr := range results {
		if sr.Err != nil {
			hadError = true
			rows = append(rows, []string{sr.Shard.LabelDB(), "?"})
			continue
		}
		var cnt int64
		if sr.Result != nil && len(sr.Result.Rows) > 0 && len(sr.Result.Rows[0]) > 0 {
			cnt = asInt64(sr.Result.Rows[0][0])
			parsed = true
		}
		total += cnt
		rows = append(rows, []string{sr.Shard.LabelDB(), fmt.Sprintf("%d", cnt)})
	}
	if !parsed {
		return !hadError
	}
	warn := ""
	if where == "" {
		warn = "  ⚠ no WHERE — affects ALL rows"
	}
	fmt.Fprintf(r.out, "impact preview — rows that match%s\n", warn)
	render.Table(r.out, []string{"shard", "would_affect"}, rows,
		fmt.Sprintf("TOTAL: %d rows would be affected", total))
	return !hadError
}
