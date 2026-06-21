package repl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jedib0t/go-pretty/v6/text"

	"terox/internal/cluster"
	"terox/internal/execution"
	"terox/internal/explain"
	"terox/internal/ui"
)

// toJSON приводит значение pgx (текст, байты или декодированное Go-значение) к JSON-строке.
func toJSON(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case []byte:
		return string(t), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

// doExplain анализирует план запроса: оценочный (безопасный), фактический
// (EXPLAIN ANALYZE, только чтение) или из сохранённого JSON-файла.
//
//	\explain <query>            оценочный план
//	\explain analyze <query>    фактический план (выполняет read-запрос)
//	\explain -f <plan.json>     анализ сохранённого плана без БД
func (r *REPL) doExplain(args []string, raw string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: \\explain [analyze] <query> | \\explain -f <plan.json>")
	}

	// Сравнение двух сохранённых планов — БД не нужна.
	if args[0] == "diff" {
		if len(args) < 3 {
			return fmt.Errorf("usage: \\explain diff <before.json> <after.json>")
		}
		return r.explainDiff(args[1], args[2])
	}

	// Из файла — БД не нужна.
	if args[0] == "-f" || args[0] == "--file" {
		if len(args) < 2 {
			return fmt.Errorf("usage: \\explain -f <plan.json>")
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		return r.renderExplain(string(data), "")
	}

	if len(r.targets) == 0 {
		return fmt.Errorf("no shard selected")
	}

	// Эталоны для проверки регрессий.
	if args[0] == "save" {
		if len(args) < 3 {
			return fmt.Errorf("usage: \\explain save <name> [analyze] <query>")
		}
		// rawTail(raw, 3) отбрасывает "\explain save <name>", оставляя запрос дословно.
		return r.explainSave(args[1], args[2:], rawTail(raw, 3))
	}
	if args[0] == "compare" {
		if len(args) < 2 {
			return fmt.Errorf("usage: \\explain compare <name>")
		}
		return r.explainCompare(args[1], args[2:])
	}

	// Исходная строка без ведущего "\explain", чтобы запрос сохранил точные пробелы
	// (литералы вроде 'a  b' не должны схлопываться).
	opts, query, err := parseExplainArgs(args, rawTail(raw, 1))
	if err != nil {
		return err
	}
	if query == "" {
		return fmt.Errorf("no query given")
	}
	if opts.analyze && execution.IsWrite(query) {
		return fmt.Errorf("EXPLAIN ANALYZE would execute this writing query — use plain \\explain (estimate) instead")
	}
	if opts.genericPlan && opts.analyze {
		return fmt.Errorf("--generic-plan cannot be combined with analyze (GENERIC_PLAN does not execute the query)")
	}
	// ANALYZE действительно ВЫПОЛНЯЕТ запрос: волатильные функции с побочными
	// эффектами сработают, а read-only ROLLBACK НЕ отменит ВНЕШНИЕ эффекты (запись
	// в файлы, dblink, отправку сигналов, advisory-локи). Предупреждаем явно (P1-3/F7).
	if opts.analyze {
		if d := execution.Classify(query); d.Level == execution.RiskVolatileSideEffect {
			fmt.Fprintln(r.out, ui.Danger.Render("⚠ EXPLAIN ANALYZE executes the query: "+d.Reasons[0]+"; the read-only ROLLBACK cannot undo external side effects"))
		}
	}
	explainSQL := explainSQLFor(opts, r.serverVersion(), query)

	// Определяем, на каких шардах выполнять explain.
	shards, err := r.explainTargets(opts)
	if err != nil {
		return err
	}

	// Один шард (или явный одношардовый режим): подробный вид.
	if len(shards) == 1 {
		planJSON, err := r.runExplain(shards[0], explainSQL, opts.analyze)
		if err != nil {
			return err
		}
		return r.renderExplain(planJSON, shards[0].Label)
	}

	// Несколько шардов: EXPLAIN ANALYZE выполняет запрос на каждом шарде, поэтому запрашиваем подтверждение.
	if opts.analyze {
		fmt.Fprintf(r.out, "EXPLAIN ANALYZE will EXECUTE this query on %d shards.\n", len(shards))
		if !r.confirmYes("proceed? [y/N] ") {
			fmt.Fprintln(r.out, "cancelled (tip: \\explain analyze --first runs a single-shard canary)")
			return nil
		}
	}
	return r.explainAcrossShards(shards, explainSQL, opts)
}

// explainOpts хранит разобранные флаги \explain.
type explainOpts struct {
	analyze     bool
	memory      bool   // MEMORY (PostgreSQL 17+)
	serialize   bool   // SERIALIZE TEXT (PostgreSQL 17+)
	genericPlan bool   // GENERIC_PLAN (PostgreSQL 16+; несовместим с ANALYZE)
	mode        string // auto, first, all, outliers, shard, sample
	shardName   string
	sample      int
}

// parseExplainArgs разбирает ведущие флаги ("analyze", "--first", "--all",
// "--outliers", "--shard <label>", "--sample N") и возвращает остаток-запрос.
// rawTail возвращает s начиная с (n+1)-го токена, разделённого пробелами, сохраняя
// исходные пробелы в остатке — в отличие от strings.Join(strings.Fields(s)),
// который схлопывает пробелы (портя литералы вроде 'a  b').
func rawTail(s string, n int) string {
	tok, prevSpace := 0, true
	for j, r := range s {
		if unicode.IsSpace(r) {
			prevSpace = true
			continue
		}
		if prevSpace {
			tok++
			if tok == n+1 {
				return s[j:]
			}
			prevSpace = false
		}
	}
	return ""
}

func parseExplainArgs(args []string, raw string) (explainOpts, string, error) {
	o := explainOpts{mode: "auto"}
	i := 0
	for i < len(args) {
		a := strings.ToLower(args[i])
		switch a {
		case "analyze", "analyse":
			o.analyze = true
		case "--memory":
			o.memory = true
		case "--serialize":
			o.serialize = true
		case "--generic-plan":
			o.genericPlan = true
		case "--first", "-1":
			o.mode = "first"
		case "--all":
			o.mode = "all"
		case "--outliers":
			o.mode = "outliers"
		case "--shard", "-s":
			if i+1 >= len(args) {
				return o, "", fmt.Errorf("--shard needs a shard label")
			}
			o.mode, o.shardName = "shard", args[i+1]
			i++
		case "--sample":
			if i+1 >= len(args) {
				return o, "", fmt.Errorf("--sample needs a count")
			}
			n, e := strconv.Atoi(args[i+1])
			if e != nil || n < 1 {
				return o, "", fmt.Errorf("--sample needs a positive count, got %q", args[i+1])
			}
			o.mode, o.sample = "sample", n
			i++
		default:
			// Первый токен не-флаг начинает запрос — берём сырой остаток отсюда,
			// чтобы внутренние пробелы (и литералы) сохранились дословно.
			return o, strings.TrimSpace(strings.TrimSuffix(rawTail(raw, i), ";")), nil
		}
		i++
	}
	return o, "", nil
}

// explainTargets определяет набор шардов для explain по опциям и текущим целям.
func (r *REPL) explainTargets(o explainOpts) ([]cluster.Shard, error) {
	switch o.mode {
	case "first":
		return r.targets[:1], nil
	case "shard":
		for _, s := range r.targets {
			if strings.EqualFold(s.Label, o.shardName) {
				return []cluster.Shard{s}, nil
			}
		}
		for _, s := range r.shards {
			if strings.EqualFold(s.Label, o.shardName) {
				return []cluster.Shard{s}, nil
			}
		}
		return nil, fmt.Errorf("shard %q not found in the current selection", o.shardName)
	case "sample":
		n := o.sample
		if n > len(r.targets) {
			n = len(r.targets)
		}
		return r.targets[:n], nil
	case "auto":
		// По умолчанию оценка по всем шардам; ANALYZE дорогой, поэтому по умолчанию
		// одношардовая проба, если не задан --all/--sample.
		if o.analyze {
			return r.targets[:1], nil
		}
		return r.targets, nil
	default: // all, outliers
		return r.targets, nil
	}
}

// runExplain выполняет EXPLAIN на одном шарде и возвращает JSON плана. Для ANALYZE
// время выполнения ограничено сессионным statement_timeout (инвариант чтения).
func (r *REPL) runExplain(shard cluster.Shard, explainSQL string, analyze bool) (string, error) {
	timeout := time.Duration(r.cfg.QueryTimeout)
	if analyze {
		r.mgr.SetReadTimeout(r.stmtTimeout)
		defer r.mgr.SetReadTimeout("")
		if st, ok := pgDurationToGo(r.stmtTimeout); ok && st+2*time.Second > timeout {
			timeout = st + 2*time.Second
		}
	}
	cctx, cancel := contextWithOptionalTimeout(context.Background(), timeout)
	defer cancel()
	res, err := r.mgr.Exec(cctx, shard, explainSQL, true)
	if err != nil {
		return "", err
	}
	if res == nil || len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return "", fmt.Errorf("no plan returned")
	}
	return toJSON(res.Rows[0][0])
}

// shardPlan — результат EXPLAIN для одного шарда.
type shardPlan struct {
	label string
	root  *explain.Root
	json  string
	err   error
}

// explainAcrossShards выполняет explain запроса на нескольких шардах, группирует
// их по структурному отпечатку плана, выделяет выбросы (структура плана и, для
// ANALYZE, время выполнения) и показывает подробную диагностику представителя.
func (r *REPL) explainAcrossShards(shards []cluster.Shard, explainSQL string, o explainOpts) error {
	timeout := time.Duration(r.cfg.QueryTimeout)
	if o.analyze {
		r.mgr.SetReadTimeout(r.stmtTimeout)
		defer r.mgr.SetReadTimeout("")
		if st, ok := pgDurationToGo(r.stmtTimeout); ok && st+2*time.Second > timeout {
			timeout = st + 2*time.Second
		}
	}
	fmt.Fprintf(r.out, "%s across %d shards…\n", sevColor("plan"), len(shards))
	results := r.fanout(shards, explainSQL, true, timeout)

	var plans []shardPlan
	var failed []string
	for _, sr := range results {
		sp := shardPlan{label: sr.Shard.Label}
		switch {
		case sr.Err != nil:
			sp.err = sr.Err
		case sr.Result == nil || len(sr.Result.Rows) == 0 || len(sr.Result.Rows[0]) == 0:
			sp.err = fmt.Errorf("no plan returned")
		default:
			if js, e := toJSON(sr.Result.Rows[0][0]); e != nil {
				sp.err = e
			} else if root, e := explain.Parse(js); e != nil {
				sp.err = e
			} else {
				sp.root, sp.json = root, js
			}
		}
		if sp.err != nil {
			failed = append(failed, sp.label)
		}
		plans = append(plans, sp)
	}

	// Группируем успешные планы по структурному отпечатку (большинство первым).
	type group struct {
		shape  string
		labels []string
		rep    shardPlan
	}
	idx := map[string]int{}
	var groups []group
	ok := 0
	for _, sp := range plans {
		if sp.root == nil {
			continue
		}
		ok++
		fp := explain.Fingerprint(sp.root)
		if gi, seen := idx[fp]; seen {
			groups[gi].labels = append(groups[gi].labels, sp.label)
		} else {
			idx[fp] = len(groups)
			groups = append(groups, group{shape: explain.Shape(sp.root), labels: []string{sp.label}, rep: sp})
		}
	}
	if ok == 0 {
		return fmt.Errorf("no plans collected (%d shard(s) failed)", len(failed))
	}
	sort.SliceStable(groups, func(i, j int) bool { return len(groups[i].labels) > len(groups[j].labels) })

	if len(groups) == 1 {
		fmt.Fprintf(r.out, "  consensus: all %d shards share one plan — %s\n", ok, groups[0].shape)
	} else {
		fmt.Fprintf(r.out, "  %d distinct plan structures across %d shards:\n", len(groups), ok)
		for gi, g := range groups {
			tag := "majority"
			if gi > 0 {
				tag = "outlier"
			}
			fmt.Fprintf(r.out, "   [%d/%d] %s  (%s)\n", len(g.labels), ok, g.shape, tag)
			if gi > 0 {
				fmt.Fprintf(r.out, "         shards: %s   → \\s %s to inspect\n",
					strings.Join(trunc(g.labels, 10), ", "), g.labels[0])
			}
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(r.out, "  %d shard(s) failed: %s\n", len(failed), strings.Join(trunc(failed, 10), ", "))
	}
	if o.analyze {
		r.reportTimeOutliers(plans)
	}

	// Подробная диагностика представительного плана: плана большинства или — для
	// --outliers — наименьшей отличающейся группы.
	rep, repTag := groups[0].rep, "majority"
	if o.mode == "outliers" && len(groups) > 1 {
		rep = groups[len(groups)-1].rep
		repTag = "outlier " + rep.label
	}
	fmt.Fprintf(r.out, "\ndetailed diagnosis (%s plan — %s):\n", repTag, rep.label)
	return r.renderExplain(rep.json, rep.label)
}

// reportTimeOutliers отмечает шарды, чьё время EXPLAIN ANALYZE сильно выше медианы
// (перекос данных даже при одинаковой структуре плана).
func (r *REPL) reportTimeOutliers(plans []shardPlan) {
	type tm struct {
		label string
		ms    float64
	}
	var ts []tm
	for _, sp := range plans {
		if sp.root != nil {
			ts = append(ts, tm{sp.label, sp.root.ExecMS()})
		}
	}
	if len(ts) < 2 {
		return
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].ms < ts[j].ms })
	// Медиана: для чётного числа выборок — среднее двух средних значений.
	n := len(ts)
	var median float64
	if n%2 == 1 {
		median = ts[n/2].ms
	} else {
		median = (ts[n/2-1].ms + ts[n/2].ms) / 2
	}
	fmt.Fprintf(r.out, "  execution time: median %.0fms, range %.0f–%.0fms\n", median, ts[0].ms, ts[len(ts)-1].ms)
	// Опорное значение для выбросов: при ≥3 выборках медиана устойчива, но при ровно
	// двух она лежит между значениями и скрыла бы большой разрыв, поэтому сравниваем
	// с более быстрым (минимальным) шардом.
	baseline := median
	if n == 2 {
		baseline = ts[0].ms
	}
	if baseline <= 0 {
		return
	}
	var slow []string
	for _, x := range ts {
		if x.ms > 3*baseline {
			slow = append(slow, fmt.Sprintf("%s %.0fms (%.1f×)", x.label, x.ms, x.ms/baseline))
		}
	}
	if len(slow) > 0 {
		fmt.Fprintf(r.out, "  %s slow outliers (>3× median): %s\n", sevTag(explain.Warning), strings.Join(slow, ", "))
	}
}

func trunc(s []string, n int) []string {
	if len(s) > n {
		return append(s[:n:n], "…")
	}
	return s
}

// confirmYes читает подтверждение y/N.
func (r *REPL) confirmYes(prompt string) bool {
	a := strings.ToLower(strings.TrimSpace(r.readLine(prompt)))
	return a == "y" || a == "yes"
}

// explainDiff сравнивает два сохранённых файла плана и печатает различия.
func (r *REPL) explainDiff(beforePath, afterPath string) error {
	bData, err := os.ReadFile(beforePath)
	if err != nil {
		return err
	}
	aData, err := os.ReadFile(afterPath)
	if err != nil {
		return err
	}
	before, err := explain.Parse(string(bData))
	if err != nil {
		return fmt.Errorf("before: %w", err)
	}
	after, err := explain.Parse(string(aData))
	if err != nil {
		return fmt.Errorf("after: %w", err)
	}
	r.renderPlanDiff(before, after)
	return nil
}

// renderPlanDiff печатает различия двух планов (before→after).
func (r *REPL) renderPlanDiff(before, after *explain.Root) {
	c := explain.Compare(before, after)

	fmt.Fprintln(r.out, sevColor("Plan comparison"))
	if c.Before.Analyzed && c.After.Analyzed {
		fmt.Fprintf(r.out, "  execution: %.1f ms → %.1f ms   %s\n",
			c.Before.ExecutionTime, c.After.ExecutionTime, pctChange(c.Before.ExecutionTime, c.After.ExecutionTime))
		fmt.Fprintf(r.out, "  rows out:  %s → %s\n", humanRows(c.Before.RowsReturned), humanRows(c.After.RowsReturned))
		if c.Before.DiskReadMB >= 1 || c.After.DiskReadMB >= 1 {
			fmt.Fprintf(r.out, "  disk read: %s → %s   %s\n",
				humanMB(c.Before.DiskReadMB), humanMB(c.After.DiskReadMB), pctChange(c.Before.DiskReadMB, c.After.DiskReadMB))
		}
		if c.Before.TempMB >= 1 || c.After.TempMB >= 1 {
			fmt.Fprintf(r.out, "  temp files: %s → %s\n", humanMB(c.Before.TempMB), humanMB(c.After.TempMB))
		}
	} else {
		fmt.Fprintln(r.out, "  (estimate-only plans; compare with EXPLAIN ANALYZE for time/IO)")
	}

	if len(c.AccessChanges) > 0 {
		fmt.Fprintln(r.out, "  access path changes:")
		for _, ch := range c.AccessChanges {
			fmt.Fprintf(r.out, "    %s\n", ch)
		}
	}
	if len(c.Resolved) > 0 {
		fmt.Fprintf(r.out, "  %s (%d):\n", sevTag(explain.Info)+" resolved", len(c.Resolved))
		for _, t := range c.Resolved {
			fmt.Fprintf(r.out, "    %s %s\n", arrow(), t)
		}
	}
	if len(c.Introduced) > 0 {
		fmt.Fprintf(r.out, "  %s (%d):\n", sevTag(explain.Warning)+" introduced", len(c.Introduced))
		for _, t := range c.Introduced {
			fmt.Fprintf(r.out, "    %s %s\n", arrow(), t)
		}
	}
	if len(c.Resolved) == 0 && len(c.Introduced) == 0 && len(c.AccessChanges) == 0 {
		fmt.Fprintln(r.out, "  no structural or issue differences detected.")
	}
}

// baselineNameRe ограничивает имя эталона простым именем файла: должно начинаться
// с буквы/цифры и содержать только буквы, цифры, '_', '-' и '.'. Это блокирует
// обход путей ("../../etc/x") и абсолютные пути, так что \explain save пишет
// только внутри каталога планов.
var baselineNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// validBaselineName защищает заданное пользователем имя эталона от обхода путей.
func validBaselineName(name string) error {
	if !baselineNameRe.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid baseline name %q (use letters, digits, '_', '-', '.'; no path separators)", name)
	}
	return nil
}

// plansDir возвращает ~/.config/terox/plans, создавая каталог.
func plansDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	d := filepath.Join(base, "terox", "plans")
	return d, os.MkdirAll(d, 0o755)
}

// explainSave строит план запроса на первой цели и сохраняет его как именованный
// эталон (JSON плана + запрос) для последующего \explain compare.
// planEnvelope фиксирует, где и как был снят эталонный план, чтобы при \explain
// compare можно было предупредить о запуске в другом окружении
// (цель/версия/режим analyze), где различия планов могут быть несущественными.
type planEnvelope struct {
	Service       string `json:"service"`
	Storage       string `json:"storage"`
	Target        string `json:"target"`
	Host          string `json:"host"`
	DB            string `json:"db"`
	Port          int    `json:"port"`
	ServerVersion int    `json:"server_version_num"`
	Analyze       bool   `json:"analyze"`
	SavedAt       string `json:"saved_at"`
}

func (r *REPL) explainSave(name string, queryArgs []string, rawQuery string) error {
	if err := validBaselineName(name); err != nil {
		return err
	}
	opts, query, err := parseExplainArgs(queryArgs, rawQuery)
	if err != nil {
		return err
	}
	if query == "" {
		return fmt.Errorf("usage: \\explain save <name> [analyze] <query>")
	}
	if opts.analyze && execution.IsWrite(query) {
		return fmt.Errorf("EXPLAIN ANALYZE would execute a writing query")
	}
	explainSQL := explainSQLFor(opts, r.serverVersion(), query)
	planJSON, err := r.runExplain(r.targets[0], explainSQL, opts.analyze)
	if err != nil {
		return err
	}
	dir, err := plansDir()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(planJSON), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+".sql"), []byte(query), 0o644); err != nil {
		return err
	}
	env := planEnvelope{
		Service: r.service, Storage: r.storage, Target: r.targets[0].Label,
		Host: r.targets[0].Host, DB: r.targets[0].DB, Port: r.targets[0].Port,
		ServerVersion: r.serverVersion(), Analyze: opts.analyze, SavedAt: r.now(),
	}
	if data, e := json.MarshalIndent(env, "", "  "); e == nil {
		_ = os.WriteFile(filepath.Join(dir, name+".env"), data, 0o644)
	}
	kind := "estimated"
	if opts.analyze {
		kind = "analyzed"
	}
	fmt.Fprintf(r.out, "saved %s baseline %q (on %s/%s/%s)\n", kind, name, r.service, r.storage, r.targets[0].Label)
	return nil
}

// baselineEnvelopeWarn печатает предупреждение, когда окружение сохранённого эталона
// отличается от текущей цели, чтобы различие планов из-за окружения не приняли за
// регрессию. По возможности: молчит, если конверт окружения не сохранён.
func (r *REPL) baselineEnvelopeWarn(dir, name string) {
	data, err := os.ReadFile(filepath.Join(dir, name+".env"))
	if err != nil {
		return
	}
	var env planEnvelope
	if json.Unmarshal(data, &env) != nil || len(r.targets) == 0 {
		return
	}
	cur := r.targets[0]
	if env.Target != "" && (env.Service != r.service || env.Storage != r.storage || env.Target != cur.Label) {
		fmt.Fprintln(r.out, ui.Dim.Render(fmt.Sprintf(
			"⚠ baseline was saved on %s/%s(%s) but you are comparing on %s/%s(%s) — differences may be environmental",
			env.Service, env.Storage, env.Target, r.service, r.storage, cur.Label)))
	}
	if env.ServerVersion != 0 && r.serverVersion() != 0 && env.ServerVersion != r.serverVersion() {
		fmt.Fprintln(r.out, ui.Dim.Render(fmt.Sprintf(
			"⚠ baseline PostgreSQL server_version_num %d differs from current %d", env.ServerVersion, r.serverVersion())))
	}
}

// explainCompare заново выполняет запрос сохранённого эталона и сравнивает новый
// план с сохранённым.
func (r *REPL) explainCompare(name string, extra []string) error {
	if err := validBaselineName(name); err != nil {
		return err
	}
	dir, err := plansDir()
	if err != nil {
		return err
	}
	baseJSON, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		return fmt.Errorf("no baseline %q (\\explain save <name> ... first)", name)
	}
	queryBytes, err := os.ReadFile(filepath.Join(dir, name+".sql"))
	if err != nil {
		return fmt.Errorf("baseline %q has no saved query", name)
	}
	r.baselineEnvelopeWarn(dir, name) // предупреждение при сравнении в другом окружении
	before, err := explain.Parse(string(baseJSON))
	if err != nil {
		return fmt.Errorf("baseline plan: %w", err)
	}
	query := strings.TrimSpace(string(queryBytes))

	// Повторный запуск с ANALYZE, чтобы сравнение включало измеренное время (запрос выполняется).
	analyze := before.Plan.ActualTotalTime != nil
	if analyze && execution.IsWrite(query) {
		return fmt.Errorf("baseline %q is not a plain read; refusing EXPLAIN ANALYZE (the saved .sql would execute a write)", name)
	}
	if analyze {
		fmt.Fprintf(r.out, "re-running baseline %q with EXPLAIN ANALYZE on %s (executes the query).\n", name, r.targets[0].Label)
		if !r.confirmYes("proceed? [y/N] ") {
			fmt.Fprintln(r.out, "cancelled")
			return nil
		}
	}
	afterJSON, err := r.runExplain(r.targets[0], explainSQLFor(explainOpts{analyze: analyze}, r.serverVersion(), query), analyze)
	if err != nil {
		return err
	}
	after, err := explain.Parse(afterJSON)
	if err != nil {
		return err
	}
	r.renderPlanDiff(before, after)
	return nil
}

// explainSQLFor строит команду EXPLAIN, включая зависящие от версии опции по версии
// сервера, чтобы работать и на PostgreSQL 11/12: SETTINGS с 12+, WAL с 13+.
// serverVer — это server_version_num (0 = неизвестно); при неизвестной версии
// версионные опции опускаются, чтобы команда не упала на старом сервере (ценой —
// потеря данных SETTINGS/WAL на новом, если определение версии не сработало).
// Версию нужно получить до вызова — см. r.serverVersion().
func explainSQLFor(o explainOpts, serverVer int, query string) string {
	var opts []string
	if o.analyze {
		opts = append(opts, "ANALYZE", "BUFFERS")
		if serverVer >= 130000 {
			opts = append(opts, "WAL") // объём WAL на узел — PostgreSQL 13+
		}
		if o.serialize && serverVer >= 170000 {
			opts = append(opts, "SERIALIZE TEXT") // время/объём сериализации вывода — 17+
		}
		if o.memory && serverVer >= 170000 {
			opts = append(opts, "MEMORY") // память планировщика — 17+
		}
	}
	if o.genericPlan && !o.analyze && serverVer >= 160000 {
		// GENERIC_PLAN строит план без значений параметров (16+); несовместим с ANALYZE.
		opts = append(opts, "GENERIC_PLAN")
	}
	opts = append(opts, "VERBOSE")
	if serverVer >= 120000 {
		opts = append(opts, "SETTINGS") // нестандартные GUC — PostgreSQL 12+
	}
	opts = append(opts, "FORMAT JSON")
	return "EXPLAIN (" + strings.Join(opts, ", ") + ") " + query
}

func pctChange(before, after float64) string {
	if before == 0 {
		if after == 0 {
			return ""
		}
		return "(new)"
	}
	d := (after - before) / before * 100
	return fmt.Sprintf("%+.1f%%", d)
}

// renderExplain разбирает JSON плана и печатает его диагностику.
func (r *REPL) renderExplain(planJSON, label string) error {
	root, err := explain.Parse(planJSON)
	if err != nil {
		return err
	}
	a := explain.AnalyzeVersion(root, r.serverVersion())

	where := ""
	if label != "" {
		where = " [" + label + "]"
	}
	fmt.Fprintf(r.out, "%s%s\n", sevColor("EXPLAIN summary"), where)
	if a.Analyzed {
		fmt.Fprintf(r.out, "  planning: %.1f ms   execution: %.1f ms\n", a.PlanningTime, a.ExecutionTime)
		fmt.Fprintf(r.out, "  rows: processed %s → returned %s\n", humanRows(a.RowsProcessed), humanRows(a.RowsReturned))
		if a.DiskReadMB >= 1 {
			fmt.Fprintf(r.out, "  disk read: %s\n", humanMB(a.DiskReadMB))
		}
		if a.TempMB >= 1 {
			fmt.Fprintf(r.out, "  temp files: %s\n", humanMB(a.TempMB))
		}
	} else {
		fmt.Fprintf(r.out, "  estimated plan (not executed); planning: %.1f ms\n", a.PlanningTime)
		fmt.Fprintln(r.out, "  evidence: estimate only — actual rows, time, buffers and spills are unavailable")
	}
	if a.MainProblem != "" {
		label := "hotspot (est. self time)"
		if !a.Analyzed {
			label = "costliest estimated branch" // суммарная стоимость; обычно корень
		}
		fmt.Fprintf(r.out, "  %s: %s\n", label, a.MainProblem)
	}
	if a.Analyzed {
		fmt.Fprintf(r.out, "  risk: %s\n", colorRisk(a.Risk))
		// Cardinality-error drill-down: топ узлов, где планировщик сильнее всего
		// ошибся с числом строк (часто корень плохих join-решений).
		if mis := explain.TopMisestimates(root, 3); len(mis) > 0 {
			fmt.Fprintln(r.out, "  worst cardinality estimates (plan vs actual):")
			for _, m := range mis {
				rel := ""
				if m.Relation != "" {
					rel = " on " + m.Relation
				}
				fmt.Fprintf(r.out, "     %s%s: est %s vs actual %s (%.0f× off)\n",
					m.Node, rel, humanRows(m.Estimated), humanRows(m.Actual), m.Ratio)
			}
		}
	} else {
		fmt.Fprintln(r.out, "  risk: unknown (estimate only — run \\explain analyze for a measured diagnosis)")
	}

	if len(a.NotEvaluated) > 0 {
		fmt.Fprintf(r.out, "  %d rule(s) not evaluated (insufficient data):\n", len(a.NotEvaluated))
		for _, ne := range a.NotEvaluated {
			fmt.Fprintf(r.out, "     %s\n", ne)
		}
	}

	if len(a.Findings) == 0 {
		if a.Analyzed {
			fmt.Fprintln(r.out, "\nno notable issues detected.")
		} else {
			fmt.Fprintln(r.out, "\nno estimate-level issues — this is NOT a health check; run \\explain analyze for actual metrics.")
		}
		return nil
	}
	fmt.Fprintf(r.out, "\n%d finding(s), by severity:\n", len(a.Findings))
	order := []string{explain.Critical, explain.Warning, explain.Info}
	for _, sev := range order {
		for _, fd := range a.Findings {
			if fd.Severity != sev {
				continue
			}
			r.renderFinding(fd)
		}
	}
	return nil
}

// renderFinding печатает одно структурированное наблюдение: факты → гипотеза →
// проверки → действия, с уверенностью и влиянием.
func (r *REPL) renderFinding(fd explain.Finding) {
	conf := ""
	if fd.Confidence > 0 {
		conf = fmt.Sprintf("  %s", ui.Dim.Render(fmt.Sprintf("[%s confidence %.0f%%, %s impact]", confWord(fd.Confidence), 100*fd.Confidence, fd.Impact)))
	}
	fmt.Fprintf(r.out, "\n  %s %s%s\n", sevTag(fd.Severity), fd.Title, conf)
	if fd.Node != "" {
		fmt.Fprintf(r.out, "     %s %s\n", ui.Dim.Render("node:"), fd.Node)
	}
	for _, ev := range fd.Evidence {
		fmt.Fprintf(r.out, "     %s %s\n", ui.Dim.Render("evidence:"), ev)
	}
	if fd.Hypothesis != "" {
		fmt.Fprintf(r.out, "     %s %s\n", ui.Dim.Render("hypothesis:"), fd.Hypothesis)
	}
	for _, ch := range fd.Checks {
		fmt.Fprintf(r.out, "     %s %s\n", ui.Dim.Render("check:"), ch)
	}
	for i, ac := range fd.Actions {
		if i == 0 {
			fmt.Fprintf(r.out, "     %s possible action (verify before applying):\n", arrow())
		}
		for _, line := range strings.Split(ac, "\n") {
			fmt.Fprintf(r.out, "       %s\n", line)
		}
	}
}

func confWord(c float64) string {
	switch {
	case c >= 0.8:
		return "high"
	case c >= 0.55:
		return "medium"
	default:
		return "low"
	}
}

func sevColor(s string) string {
	if ui.Enabled {
		return text.FgHiGreen.Sprint(s)
	}
	return s
}

func sevTag(sev string) string {
	if !ui.Enabled {
		return "[" + sev + "]"
	}
	switch sev {
	case explain.Critical:
		return text.FgRed.Sprint("[CRITICAL]")
	case explain.Warning:
		return text.FgYellow.Sprint("[WARNING]")
	default:
		return text.FgCyan.Sprint("[INFO]")
	}
}

func colorRisk(risk string) string {
	if !ui.Enabled {
		return risk
	}
	switch risk {
	case "high":
		return text.FgRed.Sprint(risk)
	case "medium":
		return text.FgYellow.Sprint(risk)
	default:
		return text.FgGreen.Sprint(risk)
	}
}

func humanRows(n float64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1fB", n/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", n/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fk", n/1e3)
	default:
		return fmt.Sprintf("%.0f", n)
	}
}

func humanMB(mb float64) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", mb/1024)
	}
	return fmt.Sprintf("%.0f MB", mb)
}

func colorStatus(s string) string {
	if !ui.Enabled {
		return s
	}
	switch s {
	case "CRITICAL":
		return text.FgRed.Sprint(s)
	case "DEGRADED":
		return text.FgYellow.Sprint(s)
	default:
		return text.FgGreen.Sprint(s)
	}
}

func arrow() string {
	if ui.Enabled {
		return text.FgGreen.Sprint("→")
	}
	return "→"
}
