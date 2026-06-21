// Package explain — разбирает вывод PostgreSQL EXPLAIN (FORMAT JSON) и
// превращает его в понятный диагноз: сводку плюс приоритизированные проблемы и
// рекомендации.
//
// Анализ опирается на широкий набор знаний по оптимизации запросов PostgreSQL:
// стоимости путей доступа, ошибки оценки строк планировщиком, выгрузки work_mem
// (сортировка, hash join, hash aggregate), использование буферов/IO и
// временных файлов, обращения к куче при index-only-scan, перепроверка bitmap,
// поздние фильтры join, повторное выполнение (циклы nested-loop,
// коррелированные SubPlan, многократно сканируемые CTE, эффективность Memoize),
// отсечение секций, параллелизм, несоответствие индексов из-за
// выражений/приведений типов, широкие строки, накладные расходы JIT и
// триггеров, перекос между планированием и выполнением.
package explain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Misestimate — узел плана с большой ошибкой оценки кардинальности (план vs факт).
type Misestimate struct {
	Node      string
	Relation  string
	Estimated float64
	Actual    float64 // loops-нормализованные фактические строки
	Ratio     float64 // max(est/actual, actual/est) — во сколько раз ошиблись
}

// TopMisestimates возвращает узлы плана с наибольшей ошибкой оценки числа строк
// (factual = Actual Rows × Loops), отсортированные по убыванию кратности ошибки.
// Только для проанализированного плана (есть Actual Rows). Порог ошибки — 10×.
// limit ограничивает число (0 = все). Чистая функция (по дереву плана).
func TopMisestimates(root *Root, limit int) []Misestimate {
	if root == nil {
		return nil
	}
	var out []Misestimate
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.ActualRows != nil {
			loops := 1.0
			if n.ActualLoops != nil && *n.ActualLoops > 0 {
				loops = *n.ActualLoops
			}
			actual := *n.ActualRows * loops
			ratio := misestimateRatio(n.PlanRows, actual)
			if ratio >= 10 {
				rel := n.RelationName
				if rel == "" {
					rel = n.Alias
				}
				out = append(out, Misestimate{n.NodeType, rel, n.PlanRows, actual, ratio})
			}
		}
		for i := range n.Plans {
			walk(&n.Plans[i])
		}
	}
	walk(&root.Plan)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ratio > out[j].Ratio })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// misestimateRatio — кратность ошибки оценки (>=1); значения <1 поднимаются до 1,
// чтобы не делить на ноль и не раздувать ошибку на крошечных числах.
func misestimateRatio(est, actual float64) float64 {
	if est < 1 {
		est = 1
	}
	if actual < 1 {
		actual = 1
	}
	if est > actual {
		return est / actual
	}
	return actual / est
}

// Node — один узел плана (широкое подмножество полей EXPLAIN).
type Node struct {
	NodeType           string  `json:"Node Type"`
	Strategy           string  `json:"Strategy"`
	ParentRelationship string  `json:"Parent Relationship"`
	JoinType           string  `json:"Join Type"`
	RelationName       string  `json:"Relation Name"`
	Schema             string  `json:"Schema"` // EXPLAIN (VERBOSE); различает одноимённые отношения из разных схем
	Alias              string  `json:"Alias"`
	IndexName          string  `json:"Index Name"`
	CTEName            string  `json:"CTE Name"`
	SubplanName        string  `json:"Subplan Name"`
	PlanRows           float64 `json:"Plan Rows"`
	PlanWidth          int     `json:"Plan Width"`
	TotalCost          float64 `json:"Total Cost"`

	ActualRows      *float64 `json:"Actual Rows"`
	ActualLoops     *float64 `json:"Actual Loops"`
	ActualTotalTime *float64 `json:"Actual Total Time"`

	RowsRemovedByFilter  *float64 `json:"Rows Removed by Filter"`
	RowsRemovedByJoin    *float64 `json:"Rows Removed by Join Filter"`
	RowsRemovedByRecheck *float64 `json:"Rows Removed by Index Recheck"`
	Filter               string   `json:"Filter"`
	JoinFilter           string   `json:"Join Filter"`
	IndexCond            string   `json:"Index Cond"`
	HashCond             string   `json:"Hash Cond"`  // условие Hash Join
	MergeCond            string   `json:"Merge Cond"` // условие Merge Join
	HeapFetches          *float64 `json:"Heap Fetches"`

	// SortKey/GroupKey — ключи Sort и (Group)Aggregate (EXPLAIN рендерит их
	// массивом строк, например ["users.created_at", "users.id DESC"]). Нужны index
	// advisor'у для предложений по сортировке/группировке.
	SortKey  []string `json:"Sort Key"`
	GroupKey []string `json:"Group Key"`

	SortMethod    string   `json:"Sort Method"`
	SortSpaceUsed *float64 `json:"Sort Space Used"` // КБ
	SortSpaceType string   `json:"Sort Space Type"`

	HashBatches  *int     `json:"Hash Batches"`
	PeakMemoryKB *float64 `json:"Peak Memory Usage"` // КБ
	DiskUsageKB  *float64 `json:"Disk Usage"`        // КБ (выгрузка hash aggregate)
	HashAggBatch *int     `json:"HashAgg Batches"`

	CacheHits   *float64 `json:"Cache Hits"`   // Memoize
	CacheMisses *float64 `json:"Cache Misses"` // Memoize

	SharedReadBlocks  *float64 `json:"Shared Read Blocks"`
	TempReadBlocks    *float64 `json:"Temp Read Blocks"`
	TempWrittenBlocks *float64 `json:"Temp Written Blocks"`

	IOReadTime  *float64 `json:"I/O Read Time"`  // мс, при track_io_timing
	IOWriteTime *float64 `json:"I/O Write Time"` // мс
	WALRecords  *float64 `json:"WAL Records"`    // EXPLAIN (WAL), PG13+
	WALFPI      *float64 `json:"WAL FPI"`
	WALBytes    *float64 `json:"WAL Bytes"`

	SubplansRemoved *int `json:"Subplans Removed"` // отсечение секций
	WorkersPlanned  *int `json:"Workers Planned"`
	WorkersLaunched *int `json:"Workers Launched"`

	Plans []Node `json:"Plans"`
}

// Trigger — измеренные накладные расходы триггера AFTER/BEFORE.
type Trigger struct {
	Name  string  `json:"Trigger Name"`
	Time  float64 `json:"Time"`
	Calls float64 `json:"Calls"`
}

// jitTiming — время компиляции JIT.
type jitTiming struct {
	Timing struct {
		Total float64 `json:"Total"`
	} `json:"Timing"`
}

// Root — один элемент массива JSON из EXPLAIN.
type Root struct {
	Plan          Node              `json:"Plan"`
	PlanningTime  float64           `json:"Planning Time"`
	ExecutionTime float64           `json:"Execution Time"`
	Triggers      []Trigger         `json:"Triggers"`
	JIT           *jitTiming        `json:"JIT"`
	Settings      map[string]string `json:"Settings"`
}

// Parse — декодирует текст EXPLAIN (FORMAT JSON) (массив из одного элемента).
func Parse(jsonText string) (*Root, error) {
	jsonText = strings.TrimSpace(jsonText)
	var roots []Root
	if err := json.Unmarshal([]byte(jsonText), &roots); err != nil {
		return nil, fmt.Errorf("parse EXPLAIN json: %w", err)
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("empty EXPLAIN output")
	}
	return &roots[0], nil
}

// Уровни серьёзности.
const (
	Critical = "CRITICAL"
	Warning  = "WARNING"
	Info     = "INFO"
)

// Finding — структурированный диагноз в модели свидетельство → гипотеза →
// проверка → действие: измеренные Evidence, вероятная причина Hypothesis (не
// факт), как её проверить (Checks) и возможные Actions, с Confidence (0..1) и
// Impact.
type Finding struct {
	RuleID     string   `json:"rule"`
	Severity   string   `json:"severity"`
	Node       string   `json:"node,omitempty"`
	Title      string   `json:"title"`
	Evidence   []string `json:"evidence,omitempty"`
	Hypothesis string   `json:"hypothesis,omitempty"`
	Checks     []string `json:"checks,omitempty"`
	Actions    []string `json:"actions,omitempty"`
	Confidence float64  `json:"confidence"`       // 0..1
	Impact     string   `json:"impact,omitempty"` // high/medium/low — потенциальная выгода
}

// Analysis — полный диагноз.
type Analysis struct {
	Analyzed      bool
	PlanningTime  float64
	ExecutionTime float64
	RowsReturned  float64
	RowsProcessed float64
	DiskReadMB    float64
	TempMB        float64
	MainProblem   string
	Risk          string
	Findings      []Finding
	// NotEvaluated перечисляет правила, которые не удалось выполнить (нет полей
	// или версии сервера), чтобы «нет проблем» не путалось с «не проверено».
	NotEvaluated []string
}

const blockMB = 8.0 / 1024.0 // блок 8 КБ в МБ

func loops(n *Node) float64 {
	if n.ActualLoops != nil && *n.ActualLoops > 0 {
		return *n.ActualLoops
	}
	return 1
}

func nodeTotalTime(n *Node) float64 {
	if n.ActualTotalTime == nil {
		return 0
	}
	return *n.ActualTotalTime * loops(n)
}

func exclusiveTime(n *Node) float64 {
	t := nodeTotalTime(n)
	for i := range n.Plans {
		t -= nodeTotalTime(&n.Plans[i])
	}
	if t < 0 {
		return 0
	}
	return t
}

func isScan(n *Node) bool {
	return strings.Contains(n.NodeType, "Scan")
}

// Analyze — строит диагноз без известной версии сервера (правила, зависящие от
// версии, проверяются по возможности).
func Analyze(root *Root) *Analysis { return AnalyzeVersion(root, 0) }

// AnalyzeVersion — обходит план и строит приоритизированный диагноз, запуская
// движок правил с учётом версии. serverVer — это server_version_num PostgreSQL
// (0 = неизвестно).
func AnalyzeVersion(root *Root, serverVer int) *Analysis {
	a := &Analysis{
		PlanningTime:  root.PlanningTime,
		ExecutionTime: root.ExecutionTime,
		Analyzed:      root.Plan.ActualTotalTime != nil,
	}
	if root.Plan.ActualRows != nil {
		a.RowsReturned = *root.Plan.ActualRows * loops(&root.Plan)
	}

	exec := root.ExecutionTime
	if exec <= 0 {
		exec = nodeTotalTime(&root.Plan)
	}

	var mainNode *Node
	var mainExcl, mainCost float64

	var walk func(n *Node)
	walk = func(n *Node) {
		if a.Analyzed {
			if e := exclusiveTime(n); e > mainExcl {
				mainExcl, mainNode = e, n
			}
		} else if n.TotalCost > mainCost {
			mainCost, mainNode = n.TotalCost, n
		}
		// Обработанные строки = строки, затронутые листовыми сканами
		// (за цикл × циклы). Исключаем Bitmap Index Scan: его строки уже учтены
		// родительским Bitmap Heap Scan, иначе двойной счёт.
		if isScan(n) && n.NodeType != "Bitmap Index Scan" && n.ActualRows != nil {
			r := *n.ActualRows
			if n.RowsRemovedByFilter != nil {
				r += *n.RowsRemovedByFilter
			}
			a.RowsProcessed += r * loops(n)
		}
		for i := range n.Plans {
			walk(&n.Plans[i])
		}
	}
	walk(&root.Plan)

	// Запускаем движок правил с учётом версии для структурированных находок.
	eng := &engine{root: root, exec: exec, serverVer: serverVer, caps: capsOf(root), notEval: map[string]string{}}
	eng.run()
	a.Findings = eng.findings
	a.NotEvaluated = eng.notEvaluatedList()

	// Счётчики BUFFERS накапливаются вверх по дереву — корень содержит весь I/O
	// запроса, поэтому берём итоги из корня (не сумму по узлам).
	if root.Plan.SharedReadBlocks != nil {
		a.DiskReadMB = *root.Plan.SharedReadBlocks * blockMB
	}
	if root.Plan.TempWrittenBlocks != nil {
		a.TempMB = *root.Plan.TempWrittenBlocks * blockMB
	}

	if mainNode != nil {
		a.MainProblem = nodeLabel(mainNode)
		if a.Analyzed && exec > 0 {
			pct := 100 * mainExcl / exec
			if pct > 100 { // время на воркер при параллелизме (Gather) может превышать реальное
				pct = 100
			}
			// «self time» здесь = total−дети, ОЦЕНКА собственной работы, а не точная
			// доля реального времени (узлы конвейерны; параллелизм добавляет эффекты воркеров).
			a.MainProblem += fmt.Sprintf("  (~%.0f%% est. self time)", pct)
		}
	}
	if a.Analyzed {
		a.Risk = riskFromFindings(a.Findings)
	} else {
		// План только с оценками не содержит фактических строк/времени/буферов/
		// выгрузок, поэтому большинство правил не выполнялось — не подразумеваем
		// измеренный вердикт.
		a.Risk = "unknown"
	}
	return a
}

// Fingerprint — возвращает каноническую структурную сигнатуру плана: типы
// узлов, типы/стратегии join и пути доступа (отношение + выбранный индекс),
// рекурсивно, игнорируя все динамические значения (стоимости, строки, время,
// буферы, литералы). Два шарда, чьи планы отличаются лишь числами, имеют
// одинаковый отпечаток; другой путь доступа (Seq Scan против Index Scan или
// другой индекс) даёт другой отпечаток. Используется для группировки шардов и
// выявления выбросов плана.
func Fingerprint(root *Root) string {
	var b strings.Builder
	var walk func(n *Node)
	walk = func(n *Node) {
		b.WriteString(n.NodeType)
		if n.Strategy != "" {
			b.WriteByte('/')
			b.WriteString(n.Strategy)
		}
		if n.JoinType != "" {
			b.WriteByte(':')
			b.WriteString(n.JoinType)
		}
		if n.RelationName != "" {
			b.WriteByte(' ')
			if n.Schema != "" {
				b.WriteString(n.Schema)
				b.WriteByte('.')
			}
			b.WriteString(n.RelationName)
		}
		if n.IndexName != "" {
			b.WriteString(" idx:")
			b.WriteString(n.IndexName)
		}
		if len(n.Plans) > 0 {
			b.WriteByte('(')
			for i := range n.Plans {
				if i > 0 {
					b.WriteByte(',')
				}
				walk(&n.Plans[i])
			}
			b.WriteByte(')')
		}
	}
	walk(&root.Plan)
	return b.String()
}

// Shape — краткое читаемое описание путей доступа плана (часть, которая обычно
// различается между шардами), например «Index Scan using items_pkey on items»
// или «Seq Scan on items + Seq Scan on orders».
func Shape(root *Root) string {
	var scans []string
	var walk func(n *Node)
	walk = func(n *Node) {
		if isScan(n) && n.NodeType != "Bitmap Index Scan" {
			scans = append(scans, scanDesc(n))
		}
		for i := range n.Plans {
			walk(&n.Plans[i])
		}
	}
	walk(&root.Plan)
	if len(scans) == 0 {
		return root.Plan.NodeType
	}
	return strings.Join(scans, " + ")
}

func scanDesc(n *Node) string {
	switch {
	case n.IndexName != "" && n.RelationName != "":
		return n.NodeType + " using " + n.IndexName + " on " + n.RelationName
	case n.IndexName != "":
		return n.NodeType + " using " + n.IndexName
	case n.RelationName != "":
		return n.NodeType + " on " + n.RelationName
	}
	return n.NodeType
}

// ExecMS — возвращает общее время выполнения (мс) для ранжирования выбросов
// шардов или 0 для плана только с оценками.
func (r *Root) ExecMS() float64 {
	if r.ExecutionTime > 0 {
		return r.ExecutionTime
	}
	return nodeTotalTime(&r.Plan)
}

func nodeLabel(n *Node) string {
	s := n.NodeType
	switch {
	case n.RelationName != "":
		s += " on " + n.RelationName
		// Добавляем алиас, если он отличается, чтобы self-join (FROM t a JOIN t b)
		// давал разные идентичности узлов («Seq Scan on t a» против «... b»), а не
		// совпадал в карте находок по узлам.
		if n.Alias != "" && n.Alias != n.RelationName {
			s += " " + n.Alias
		}
	case n.IndexName != "":
		s += " using " + n.IndexName
	case n.CTEName != "":
		s += " on " + n.CTEName
	}
	return s
}

func relOr(n *Node, def string) string {
	if n.RelationName != "" {
		return n.RelationName
	}
	return def
}

// quoteIdentSuggest оборачивает идентификатор в кавычки, если он не простой
// строчный — чтобы DDL-подсказку с необычным именем таблицы можно было скопировать
// в БД без правок.
func quoteIdentSuggest(s string) string {
	simple := s != ""
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '$'
		if i == 0 {
			ok = c == '_' || (c >= 'a' && c <= 'z')
		}
		if !ok {
			simple = false
			break
		}
	}
	if simple {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// relSuggest — имя отношения для DDL-подсказки: в кавычках при необходимости,
// либо def, если имя неизвестно.
func relSuggest(n *Node, def string) string {
	if n.RelationName == "" {
		return def
	}
	return quoteIdentSuggest(n.RelationName)
}

func filterIndexSuggestion(n *Node) string {
	col := "<column>"
	if n.Filter != "" {
		col = n.Filter
	}
	return fmt.Sprintf("consider an index covering the filter (a partial index if it selects a small fraction):\n"+
		"  CREATE INDEX CONCURRENTLY ON %s (...)  WHERE %s;", relSuggest(n, "<table>"), col)
}

// exprIndexHint — сообщает, применяет ли предикат функцию/приведение к столбцу.
func exprIndexHint(cond string) bool {
	l := strings.ToLower(cond)
	if strings.TrimSpace(l) == "" {
		return false
	}
	// Надёжный признак несоответствия индексу — только обёртки вызова функции
	// над столбцом. Голые приведения (::text) и «((» встречаются в обычных
	// предикатах/приведениях литералов и дают шумные ложные срабатывания.
	for _, p := range []string{"lower(", "upper(", "coalesce(", "date_trunc(", "to_char("} {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

func condSnippet(cond string) string {
	cond = strings.TrimSpace(cond)
	// Режем по рунам, а не по байтам: предикат EXPLAIN может содержать не-ASCII
	// литералы/идентификаторы (кириллица, эмодзи), и срез по байту порвал бы UTF-8.
	if r := []rune(cond); len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return cond
}

func timeNote(share float64) string {
	if share <= 0 {
		return ""
	}
	return fmt.Sprintf(" — %.0f%% of total time on this node", 100*share)
}

// Comparison — разница между двумя планами (до и после).
type Comparison struct {
	Before, After *Analysis
	AccessChanges []string // например «events: Seq Scan → Index Scan»
	Resolved      []string // заголовки проблем, бывших до, но отсутствующих после
	Introduced    []string // заголовки проблем, отсутствовавших до, но появившихся после
}

// scanRelKey — идентифицирует сканируемое отношение для сравнения планов: с
// квалификацией по схеме (чтобы одноимённые отношения из разных схем не
// сталкивались) и с суффиксом-алиасом, когда он отличается от имени отношения
// (чтобы self-join `FROM t a JOIN t b` записывал ОБА скана, а не только первый).
func scanRelKey(n *Node) string {
	rel := n.RelationName
	if n.Schema != "" {
		rel = n.Schema + "." + rel
	}
	if n.Alias != "" && n.Alias != n.RelationName {
		rel += " " + n.Alias
	}
	return rel
}

// scanMap — записывает по каждому отношению (с квалификацией по схеме и алиасу)
// использованный путь доступа: тип узла скана И конкретный индекс, чтобы смена
// индекса за Index/Index Only/Bitmap сканом («Index Scan using idx_a» → «...
// idx_b») попадала в AccessChanges, а не скрывалась за неизменным типом узла.
func scanMap(n *Node, m map[string]string) {
	if n.RelationName != "" && strings.Contains(n.NodeType, "Scan") {
		if k := scanRelKey(n); k != "" {
			if _, ok := m[k]; !ok {
				val := n.NodeType
				if n.IndexName != "" {
					val += " using " + n.IndexName
				}
				m[k] = val
			}
		}
	}
	for i := range n.Plans {
		scanMap(&n.Plans[i], m)
	}
}

// findingKeys — ключует находки по стабильному ID правила + идентичности узла
// (не по отображаемому заголовку, в котором динамические числа), чтобы
// resolved/introduced были устойчивы.
func findingKeys(a *Analysis) map[string]string {
	s := map[string]string{}
	for _, f := range a.Findings {
		s[f.RuleID+"\x00"+f.Node] = f.Title
	}
	return s
}

// Compare — сравнивает два плана: дельты метрик, изменения путей доступа и какие
// проблемы устранены или появились. Связывает отношения между двумя деревьями,
// даже если их структура изменилась.
func Compare(before, after *Root) *Comparison {
	c := &Comparison{Before: Analyze(before), After: Analyze(after)}

	bm, am := map[string]string{}, map[string]string{}
	scanMap(&before.Plan, bm)
	scanMap(&after.Plan, am)

	rels := map[string]bool{}
	for r := range bm {
		rels[r] = true
	}
	for r := range am {
		rels[r] = true
	}
	names := make([]string, 0, len(rels))
	for r := range rels {
		names = append(names, r)
	}
	sortStrings(names)
	for _, rel := range names {
		b, a := bm[rel], am[rel]
		switch {
		case b != "" && a != "" && b != a:
			c.AccessChanges = append(c.AccessChanges, fmt.Sprintf("%s: %s → %s", rel, b, a))
		case b == "" && a != "":
			c.AccessChanges = append(c.AccessChanges, fmt.Sprintf("%s: (new) %s", rel, a))
		case b != "" && a == "":
			c.AccessChanges = append(c.AccessChanges, fmt.Sprintf("%s: %s → (gone)", rel, b))
		}
	}

	bk, ak := findingKeys(c.Before), findingKeys(c.After)
	for k, title := range bk {
		if _, still := ak[k]; !still {
			c.Resolved = append(c.Resolved, title)
		}
	}
	for k, title := range ak {
		if _, had := bk[k]; !had {
			c.Introduced = append(c.Introduced, title)
		}
	}
	sortStrings(c.Resolved)
	sortStrings(c.Introduced)
	return c
}

// sortStrings — сортирует на месте (небольшой локальный помощник, чтобы не
// добавлять лишний импорт).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func riskFromFindings(fs []Finding) string {
	risk := "low"
	for _, f := range fs {
		if f.Severity == Critical {
			return "high"
		}
		if f.Severity == Warning {
			risk = "medium"
		}
	}
	return risk
}
