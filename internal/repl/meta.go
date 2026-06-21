package repl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"terox/internal/cluster"
	"terox/internal/db"
	"terox/internal/execution"
	"terox/internal/migration"
	"terox/internal/ui"
	"terox/internal/wizard"
)

// errSelectQuit сигнализирует, что пользователь вышел из верхнего стартового меню.
var errSelectQuit = fmt.Errorf("selection cancelled")

// selectContext выбирает service -> storage -> shard через интерактивные меню.
// "← back" (или Esc) поднимает на уровень выше; Esc на верхнем уровне возвращает
// errSelectQuit. Непустой presetService начинает сразу с шага storage для этого
// сервиса (используется \use <service>).
func (r *REPL) selectContext(presetService string) error {
	services := r.cfg.ServiceNames()
	if len(services) == 0 {
		return fmt.Errorf("no services configured")
	}
	const back = "← back"
	level := 0
	var service, storage string
	if presetService != "" {
		if svc, ok := r.cfg.Services[presetService]; !ok || svc == nil {
			return fmt.Errorf("unknown service %q", presetService)
		}
		service, level = presetService, 1
	}
	for {
		switch level {
		case 0: // сервис
			if len(services) == 1 {
				service, level = services[0], 1
				continue
			}
			s, aborted, err := huhSelect("Service", services)
			if err != nil {
				return err
			}
			if aborted {
				return errSelectQuit // выше верхнего уровня ничего нет
			}
			service, level = s, 1
		case 1: // хранилище
			storages := r.cfg.StorageNames(service)
			if len(storages) == 0 {
				return fmt.Errorf("service %q has no storages", service)
			}
			opts := append([]string{back}, storages...)
			s, aborted, err := huhSelect("Storage ("+service+")  ·  Esc = back", opts)
			if err != nil {
				return err
			}
			if aborted || s == back {
				if len(services) == 1 {
					return errSelectQuit // единственный сервис — выше storage ничего нет
				}
				level = 0
				continue
			}
			storage, level = s, 2
		case 2: // шард
			if err := r.bindStorage(service, storage); err != nil {
				return err
			}
			goBack, err := r.selectShardMenu(true)
			if err != nil {
				return err
			}
			if goBack {
				level = 1
				continue
			}
			return nil
		}
	}
}

// bindStorage устанавливает активные service/storage, разворачивает шарды и
// сбрасывает кэши автодополнения. Подмножество шардов не выбирает.
func (r *REPL) bindStorage(service, storage string) error {
	svc, ok := r.cfg.Services[service]
	if !ok || svc == nil {
		return fmt.Errorf("unknown service %q", service)
	}
	st, ok := svc.Storages[storage]
	if !ok || st == nil {
		return fmt.Errorf("unknown or empty storage %q in service %q", storage, service)
	}
	shards, err := cluster.Expand(st)
	if err != nil {
		return err
	}
	switched := r.storage != "" && (r.service != service || r.storage != storage)
	r.service = service
	r.storage = storage
	r.shards = shards
	r.prod = st.Prod
	r.migrationRole = st.MigrationRole
	// Режим записи привязан к контексту: смена storage/service сбрасывает в
	// read-only, чтобы запись не перенеслась на другой (прод) кластер.
	// Переназначение шардов в том же storage (\shard) его сохраняет.
	if switched && r.writeMode {
		r.writeMode = false
		fmt.Fprintln(r.out, ui.Dim.Render("write mode reset to read-only (context changed)"))
	}
	// В новом storage схема может отличаться — сбрасываем каталог автодополнения
	// (и отменяем фоновую загрузку, если она идёт).
	r.resetCatalog()
	// Смена контекста делает недействительными последний экспортируемый результат
	// и его запрос: \export не должен повторять на новой цели запрос, снятый на
	// прежнем storage/шарде.
	r.lastQuery = ""
	r.clearLastResult()
	return nil
}

// ctxSnapshot сохраняет активный контекст для восстановления при отмене.
type ctxSnapshot struct {
	service, storage, targetLabel string
	shards, targets               []cluster.Shard
	prod                          bool
	migrationRole                 string
	// writeMode сохраняется тоже: bindStorage сбрасывает его в read-only при смене
	// контекста, поэтому при отмене (\use/\connect) его нужно восстановить — иначе
	// откат вернул бы старый storage, но молча оставил запись выключенной.
	writeMode bool
}

func (r *REPL) snapshotContext() ctxSnapshot {
	return ctxSnapshot{r.service, r.storage, r.targetLabel, r.shards, r.targets, r.prod, r.migrationRole, r.writeMode}
}

func (r *REPL) restoreContext(s ctxSnapshot) {
	r.service, r.storage, r.targetLabel = s.service, s.storage, s.targetLabel
	r.shards, r.targets, r.prod = s.shards, s.targets, s.prod
	r.migrationRole = s.migrationRole
	r.writeMode = s.writeMode
	// Каталог автодополнения сброшен во время выбора; сбрасываем его снова, чтобы
	// он перезагрузился для восстановленного storage.
	r.resetCatalog()
}

// loadStorage привязывает storage и определяет подмножество шардов по селектору.
func (r *REPL) loadStorage(service, storage, shardSel string) error {
	if err := r.bindStorage(service, storage); err != nil {
		return err
	}
	return r.resolveTargets(shardSel)
}

// selectShardMenu показывает интерактивный выбор шарда с опциональным "← back".
func (r *REPL) selectShardMenu(allowBack bool) (goBack bool, err error) {
	if len(r.shards) == 1 {
		r.targets = r.shards
		r.targetLabel = r.shards[0].Label
		r.healthStale = true
		return false, nil
	}
	const backItem = "← back"
	opts := []string{"all", "custom (e.g. 0,1,3..7)"}
	opts = append(opts, shardMenuLabels(r.shards)...)
	if allowBack {
		opts = append([]string{backItem}, opts...)
	}
	title := fmt.Sprintf("Shard — %s/%s (%d total)", r.service, r.storage, len(r.shards))
	if allowBack {
		title += "  ·  Esc = back"
	}
	choice, aborted, err := huhSelect(title, opts)
	if err != nil {
		return false, err
	}
	if (aborted && allowBack) || choice == backItem {
		return true, nil
	}
	if aborted {
		return false, fmt.Errorf("cancelled")
	}
	sel := shardSelectorFromChoice(choice)
	if strings.HasPrefix(choice, "custom") {
		sel = r.readLine("shards (e.g. 0,1,3..7 or all): ")
	}
	targets, label, perr := cluster.ParseSelector(r.shards, sel)
	if perr != nil {
		return false, perr
	}
	r.targets, r.targetLabel = targets, label
	r.healthStale = true
	return false, nil
}

// applyTarget разбирает строку "service/storage/selector" и загружает её без
// интерактивных запросов. При отсутствии selector используется "all".
func (r *REPL) applyTarget(target string) error {
	parts := strings.SplitN(target, "/", 3)
	if len(parts) < 2 {
		return fmt.Errorf("target must be service/storage[/selector], got %q", target)
	}
	sel := "all"
	if len(parts) == 3 && strings.TrimSpace(parts[2]) != "" {
		sel = parts[2]
	}
	return r.loadStorage(parts[0], parts[1], sel)
}

// retarget меняет только подмножество шардов в текущем storage. Быстрый путь для
// сценария «запрос по всем шардам, данные на rs042 — перейти туда».
func (r *REPL) retarget(sel string) error {
	if len(r.shards) == 0 {
		return fmt.Errorf("no storage selected")
	}
	if err := r.resolveTargets(sel); err != nil {
		return err
	}
	// Переназначение шардов делает недействительным последний экспортируемый
	// результат (снятый на прежнем подмножестве), чтобы \export не отдал его на
	// новые цели.
	r.lastQuery = ""
	r.clearLastResult()
	fmt.Fprintf(r.out, "→ %s\n", r.contextLabel())
	return nil
}

// resolveTargets заполняет r.targets/r.targetLabel по селектору; при пустом sel и
// нескольких шардах в storage показывает интерактивный выбор.
func (r *REPL) resolveTargets(sel string) error {
	// Storage с одним шардом: привязываем напрямую.
	if len(r.shards) == 1 {
		r.targets = r.shards
		r.targetLabel = r.shards[0].Label
		r.healthStale = true
		return nil
	}

	if sel == "" {
		opts := append([]string{"all", "custom (e.g. 0,1,3..7)"}, shardMenuLabels(r.shards)...)
		choice, aborted, err := huhSelect(fmt.Sprintf("Shard (%d total)", len(r.shards)), opts)
		if err != nil {
			return err
		}
		if aborted {
			return fmt.Errorf("cancelled")
		}
		if strings.HasPrefix(choice, "custom") {
			sel = r.readLine("shards (e.g. 0,1,3..7 or all): ")
		} else {
			sel = shardSelectorFromChoice(choice)
		}
	}

	targets, label, err := cluster.ParseSelector(r.shards, sel)
	if err != nil {
		return err
	}
	r.targets = targets
	r.targetLabel = label
	r.healthStale = true
	return nil
}

// shardMenuLabels форматирует каждый шард для меню как "label/db" (чтобы база
// была видна, как в приглашении), иначе — просто метка.
func shardMenuLabels(shards []cluster.Shard) []string {
	out := make([]string, len(shards))
	for i, s := range shards {
		out[i] = labelWithDB(s)
	}
	return out
}

// shardSelectorFromChoice извлекает селектор (метку шарда) из выбора меню, который
// может содержать суффикс "/db". В метках шардов нет '/', поэтому метка — это
// часть до первого '/'; "all" и "← back" проходят как есть.
func shardSelectorFromChoice(choice string) string {
	if i := strings.IndexByte(choice, '/'); i >= 0 {
		return choice[:i]
	}
	return choice
}

// runMeta выполняет backslash-команду. Возвращает quit=true для выхода из цикла.
func (r *REPL) runMeta(line string) (quit bool, err error) {
	fields := strings.Fields(line)
	cmd := fields[0]
	args := fields[1:]
	// qargs — разбиение с учётом кавычек для команд, чьи аргументы являются путями
	// (путь с пробелами можно задать в "..."). Команды с SQL-хвостом используют
	// обычный Fields (кавычки там — часть SQL).
	qargs := tokenizeArgs(line)
	if len(qargs) > 0 {
		qargs = qargs[1:]
	}

	switch cmd {
	case "\\q", "\\quit":
		if r.writeMode {
			fmt.Fprintln(r.out, ui.Dim.Render("(left in write mode)"))
		}
		return true, nil

	case "\\?", "\\help":
		r.printHelp(args)

	case "\\use":
		// Полный выбор с возвратом: service -> storage -> shard (Esc поднимает выше).
		preset := ""
		if len(args) > 0 {
			preset = args[0]
		}
		snap := r.snapshotContext()
		if err := r.selectContext(preset); err != nil {
			if errors.Is(err, errSelectQuit) {
				r.restoreContext(snap) // отмена — сохраняем текущий контекст
				return false, nil
			}
			r.restoreContext(snap)
			return false, err
		}
		fmt.Fprintf(r.out, "→ %s\n", r.contextLabel())

	case "\\c", "\\connect":
		if len(args) == 0 {
			return false, fmt.Errorf("usage: \\c <storage> [shard|all]")
		}
		shardSel := ""
		if len(args) > 1 {
			// Запятая — естественный разделитель селектора: склейка токенов делает
			// "\c storage 0 1" эквивалентом "\c storage 0,1", а не молча отбрасывает
			// всё после первого токена (что незаметно сузило бы набор целей).
			shardSel = strings.Join(args[1:], ",")
		}
		// Транзакционно: восстанавливаем контекст при неверном или отменённом селекторе.
		snap := r.snapshotContext()
		if err := r.loadStorage(r.service, args[0], shardSel); err != nil {
			r.restoreContext(snap)
			return false, err
		}

	case "\\l", "\\list":
		r.printList()

	case "\\shards":
		r.printShards()

	case "\\shard", "\\s":
		// Быстрое переназначение в текущем storage. Без аргумента открывает меню.
		// Удобно сразу после запроса по всем шардам, когда видно, на каком шарде
		// данные — перейти прямо туда.
		sel := ""
		if len(args) > 0 {
			// Запятая — естественный разделитель селектора (0,3 / 0-3,7): склейка
			// токенов через неё делает "\shard 0 3" эквивалентом "\shard 0,3", а не
			// молча неверной позиции "03".
			sel = strings.Join(args, ",")
		}
		return false, r.retarget(sel)

	case "\\add":
		svc, sto, e := wizard.Run(r.cfg)
		if e != nil {
			return false, e
		}
		fmt.Fprintf(r.out, "Added %s/%s. Switch with \\use %s\n", svc, sto, svc)

	case "\\write":
		if len(args) == 0 {
			fmt.Fprintf(r.out, "write mode is %s\n", onOff(r.writeMode))
			return false, nil
		}
		// Строгое число аргументов: лишний токен не должен молча включить опасный
		// режим (например, `\write on oops`).
		if len(args) > 1 {
			return false, fmt.Errorf("usage: \\write on|off")
		}
		switch strings.ToLower(args[0]) {
		case "on":
			r.writeMode = true
		case "off":
			r.writeMode = false
		default:
			return false, fmt.Errorf("usage: \\write on|off")
		}
		fmt.Fprintf(r.out, "write mode %s\n", onOff(r.writeMode))

	case "\\timing":
		on, e := parseOnOff(args, r.timing)
		if e != nil {
			return false, fmt.Errorf("\\timing: %w", e)
		}
		r.timing = on
		fmt.Fprintf(r.out, "timing %s\n", onOff(r.timing))

	case "\\maxrows":
		if len(args) == 0 {
			fmt.Fprintf(r.out, "maxrows = %s\n", maxRowsLabel(r.maxRows))
			return false, nil
		}
		n, e := parseMaxRows(args[0])
		if e != nil {
			return false, e
		}
		r.maxRows = n
		fmt.Fprintf(r.out, "maxrows = %s\n", maxRowsLabel(r.maxRows))

	case "\\h", "\\history":
		r.runHistory(args)

	case "\\e", "\\edit":
		// Открываем редактор, заранее заполненный последним запросом (psql-стиль):
		// большой запрос удобно править и перезапускать, не набирая заново.
		sql, e := r.editInEditor(r.lastQuery)
		if e != nil {
			return false, e
		}
		if strings.TrimSpace(sql) != "" {
			r.recordHistory(strings.TrimSpace(sql)) // не пишем секреты ни в диск, ни в память
			r.runStatement(strings.TrimSuffix(strings.TrimSpace(sql), ";"))
		}

	case "\\i", "\\include":
		o, err := parseFileArgs(qargs)
		if err != nil {
			return false, fmt.Errorf("usage: \\i [--allowed] [--resume|--canary|--batch N] [--max-lag D] [--force] <file.sql>")
		}
		// Прямая передача: файл отправляется как один exec без изменений (begin/commit
		// и role/timeout на вас). Concurrent-операторы определяются автоматически.
		return false, r.runMigrationFile(false, o)

	case "\\migrate", "\\m":
		o, err := parseFileArgs(qargs)
		if err != nil {
			return false, fmt.Errorf("usage: \\migrate [--allowed] [--resume|--canary|--batch N] [--max-lag D] [--force] <file.sql>")
		}
		// Режим тела: terox оборачивает set role + statement_timeout из сессии.
		return false, r.runMigrationFile(true, o)

	case "\\timeout":
		return false, r.setTimeout(args)

	case "\\dt":
		r.runStatement(listTablesSQL)

	case "\\dn":
		r.runStatement(listSchemasSQL)

	case "\\di":
		r.runStatement(listIndexesSQL)

	case "\\d":
		if len(args) == 0 {
			r.runStatement(listTablesSQL)
		} else {
			// Структура таблицы на первом шарде: колонки + индексы/FK/referenced-by/
			// check/size. Для межшардового дрейфа — \diff.
			return false, r.doDescribe(args[0])
		}

	case "\\count":
		return false, r.doCount(args, line)

	case "\\locate", "\\find":
		return false, r.doLocate(args, line)

	case "\\diff":
		return false, r.doDiff(args)

	case "\\ping":
		r.doPing()

	case "\\activity":
		r.doActivity(args)

	case "\\locks":
		r.doLocks()

	case "\\blockers":
		r.doBlockers(args)

	case "\\longtx":
		r.doLongtx(args)

	case "\\statements", "\\workload":
		r.doStatements(args)

	case "\\copy":
		return false, r.doCopy(line)

	case "\\advise":
		return false, r.doAdvise(line)

	case "\\lint":
		return false, r.doLint(line)

	case "\\cancel":
		return false, r.doSignalBackend(args, false)

	case "\\terminate":
		return false, r.doSignalBackend(args, true)

	case "\\explain":
		return false, r.doExplain(args, line)

	case "\\doctor":
		return false, r.doDoctor(args)

	case "\\heal":
		return false, r.doHeal(args)

	case "\\completion":
		r.doCompletion(args)

	case "\\editor":
		return false, r.doEditor(args)

	case "\\layout":
		return false, r.doLayout(args)

	case "\\compare":
		return false, r.doCompare(args)

	case "\\export":
		return false, r.doExport(qargs)

	case "\\save":
		return false, r.doSave(args, line)

	case "\\run":
		return false, r.doRun(args)

	case "\\queries":
		r.doQueries()

	case "\\unsave":
		return false, r.doUnsave(args)

	case "\\x":
		r.expanded = !r.expanded
		fmt.Fprintf(r.out, "expanded display %s\n", onOff(r.expanded))

	case "\\impact":
		on, e := parseOnOff(args, r.impact)
		if e != nil {
			return false, fmt.Errorf("\\impact: %w", e)
		}
		r.impact = on
		fmt.Fprintf(r.out, "write impact preview %s\n", onOff(r.impact))

	case "\\suggest":
		on, e := parseOnOff(args, r.suggest)
		if e != nil {
			return false, fmt.Errorf("\\suggest: %w", e)
		}
		r.suggest = on
		fmt.Fprintf(r.out, "inline suggestions %s\n", onOff(r.suggest))

	case "\\write_approve":
		on, e := parseOnOff(args, r.writeApprove)
		if e != nil {
			return false, fmt.Errorf("\\write_approve: %w", e)
		}
		r.writeApprove = on
		fmt.Fprintf(r.out, "подтверждение записи %s\n", onOff(r.writeApprove))
		if !r.writeApprove {
			fmt.Fprintln(r.out, ui.Dim.Render("  записи выполняются без вопроса — осторожно"))
		}

	case "\\watch":
		return false, r.doWatch(args, line)

	case "\\g":
		return false, r.doRepeat(args, false)

	case "\\gx":
		return false, r.doRepeat(args, true)

	case "\\grep":
		return false, r.doGrep(args)

	case "\\sizes":
		return false, r.doSizes(args)

	default:
		if sug := suggestMeta(cmd); sug != "" {
			return false, fmt.Errorf("unknown command %q — did you mean %s? (try \\?)", cmd, sug)
		}
		return false, fmt.Errorf("unknown command %q (try \\?)", cmd)
	}
	return false, nil
}

// doRepeat (\g / \gx) повторяет последний выполненный запрос. С аргументом-селектором
// временно сужает цели на это подмножество шардов (\g rs042), затем восстанавливает
// прежние. \gx показывает результат в expanded-режиме. Запрос идёт через runStatement,
// поэтому запись была бы заново про-гейчена (режим записи + подтверждение), а не
// повторена молча.
func (r *REPL) doRepeat(args []string, expanded bool) error {
	if strings.TrimSpace(r.lastQuery) == "" {
		return fmt.Errorf("no previous query to repeat")
	}
	sql := r.lastQuery
	if len(args) > 0 {
		if len(r.shards) == 0 {
			return fmt.Errorf("no storage selected")
		}
		targets, label, err := cluster.ParseSelector(r.shards, strings.Join(args, ","))
		if err != nil {
			return err
		}
		savedTargets, savedLabel := r.targets, r.targetLabel
		r.targets, r.targetLabel = targets, label
		defer func() { r.targets, r.targetLabel = savedTargets, savedLabel }()
	}
	if expanded {
		savedExpanded := r.expanded
		r.expanded = true
		defer func() { r.expanded = savedExpanded }()
	}
	r.runStatement(sql)
	return nil
}

// suggestMeta предлагает ближайшие мета-команды к опечатке (расстояние Левенштейна,
// порог зависит от длины токена, чтобы короткие \s/\x/\e не переоверматчивали) — для
// подсказки «did you mean». Возвращает до трёх кандидатов через запятую или "".
func suggestMeta(cmd string) string {
	cmd = strings.ToLower(strings.TrimSpace(cmd))
	maxDist := 2
	if len(cmd) <= 4 { // вместе с ведущим '\' это команды вроде \s, \x, \dt
		maxDist = 1
	}
	type cand struct {
		name string
		d    int
	}
	var cands []cand
	for _, m := range metaCommands {
		if d := levenshtein(cmd, strings.ToLower(m)); d <= maxDist {
			cands = append(cands, cand{m, d})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].d != cands[j].d {
			return cands[i].d < cands[j].d
		}
		return cands[i].name < cands[j].name
	})
	if len(cands) > 3 {
		cands = cands[:3]
	}
	names := make([]string, len(cands))
	for i, c := range cands {
		names[i] = c.name
	}
	return strings.Join(names, ", ")
}

// levenshtein — расстояние редактирования между двумя строками (по рунам).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// migrateOpts — разобранные флаги \migrate/\i, включая поэтапную раскатку (Feature 11).
type migrateOpts struct {
	path   string
	dryRun bool
	resume bool          // применять только ещё не применённые шарды (по ledger)
	canary bool          // первый этап — один шард (проверка), затем остальные
	batch  int           // размер батча (0 = все оставшиеся одним этапом)
	force  bool          // не блокировать при дрейфе контрольной суммы
	maxLag time.Duration // порог задержки репликации между этапами (0 = выкл, F11+)
	check  bool          // только migration-aware lint (статика, без БД), без применения
}

// staged сообщает, запрошена ли поэтапная раскатка (resume/canary/batch).
func (o migrateOpts) staged() bool { return o.resume || o.canary || o.batch > 0 }

// parseFileArgs извлекает путь к файлу и флаги. Миграции по умолчанию идут в DRY-RUN
// (печатают точный exec); для реального запуска нужен --allowed. Флаги раскатки:
// --resume (только незавершённые шарды), --canary (первый этап — один шард), --batch N
// (батчи по N с барьером), --force (игнорировать дрейф контрольной суммы).
func parseFileArgs(args []string) (migrateOpts, error) {
	o := migrateOpts{dryRun: true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--allowed":
			o.dryRun = false
		case "--dry-run":
			o.dryRun = true
		case "--resume":
			o.resume = true
		case "--canary":
			o.canary = true
		case "--force":
			o.force = true
		case "--check":
			o.check = true
		case "--batch":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--batch needs a positive count")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				return o, fmt.Errorf("--batch needs a positive count, got %q", args[i])
			}
			o.batch = n
		case "--max-lag":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--max-lag needs a duration (e.g. 30s)")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil || d < 0 {
				return o, fmt.Errorf("--max-lag needs a non-negative duration, got %q", args[i])
			}
			o.maxLag = d
		default:
			if o.path != "" {
				return o, fmt.Errorf("too many arguments")
			}
			o.path = a
		}
	}
	if o.path == "" {
		return o, fmt.Errorf("missing file")
	}
	return o, nil
}

// runMigrationFile выполняет .sql-файл на текущих целях. wrap=true строит
// защитную обёртку из тела файла (\migrate); wrap=false шлёт файл как один exec
// без изменений (\i). o.dryRun печатает точный payload, ничего не выполняя; флаги
// раскатки (resume/canary/batch) включают поэтапное применение (Feature 11).
func (r *REPL) runMigrationFile(wrap bool, o migrateOpts) error {
	data, err := os.ReadFile(o.path)
	if err != nil {
		return err
	}
	content := string(data)
	plan := migration.Classify(content)

	// --check: только статический migration-aware lint (без БД, без применения).
	if o.check {
		r.printMigrationLint(content)
		return nil
	}

	if o.dryRun {
		r.previewMigration(content, wrap, plan)
		if o.staged() {
			r.previewRollout(o)
		}
		// migration-aware lint в составе превью (опасные онлайн-паттерны).
		r.printMigrationLint(content)
		fmt.Fprintln(r.out, ui.Dim.Render("— dry-run (default). Re-run with --allowed to actually apply. —"))
		return nil
	}
	if plan.Mixed {
		return fmt.Errorf("mixed file: split CONCURRENTLY/VACUUM statements into a separate file (they cannot share a transaction)")
	}
	if !r.writeMode {
		return fmt.Errorf("migrations require write mode (\\write on)")
	}
	// \migrate оборачивает тело; тело со своими begin/commit или set role вышло бы
	// из обёртки — отказываем до подтверждения. \i (wrap=false) шлёт как есть и
	// этого ограничения не имеет.
	if wrap && migration.HasTxControl(content) {
		r.refuseTxControl()
		return nil
	}
	// \migrate оборачивает тело: session-scoped конструкции (SET search_path,
	// TEMP, LISTEN, PREPARE, cursors, session advisory locks, DISCARD) пережили бы
	// COMMIT обёртки — отклоняем. \i (wrap=false) шлёт файл дословно и не
	// проходит этот фильтр (сессией владеет оператор).
	if wrap {
		if reason := migration.SessionStateViolation(content); reason != "" {
			r.refuseSessionState(reason)
			return nil
		}
	}

	// Ledger checksum drift: если миграция с этим именем уже применялась в этом
	// контексте с ДРУГИМ содержимым — предупреждаем И БЛОКИРУЕМ (если не --force):
	// отредактированный файл под тем же именем легко переиграть случайно (Feature 11).
	name := filepath.Base(o.path)
	r.warnChecksumDrift(name, content)
	if r.checksumMismatch(name, content) {
		// --resume сверяет журнал по ИМЕНИ и не знает о содержимом: при дрейфе он
		// пропустил бы «уже применённые» шарды со старым телом, и новая версия не
		// легла бы НИКУДА. --force это не лечит — отказываем безусловно.
		if o.resume {
			return fmt.Errorf("refusing --resume of %s: the file changed since it was last applied (checksum mismatch) — resume can't tell which shards already have the new version; rename the migration or re-apply it without --resume", name)
		}
		if !o.force {
			return fmt.Errorf("refusing to apply %s: checksum mismatch with the previously applied version — re-check, rename the migration, or pass --force to override", name)
		}
	}

	// Поэтапная раскатка (resume/canary/batch): применяем шарды этапами с барьером.
	if o.staged() {
		return r.runStagedRollout(wrap, content, name, plan, o)
	}

	if plan.NonTransactional {
		fmt.Fprintf(r.out, "%d non-transactional statement(s) from %s → %d shard(s) [%s] (separate execs, no wrapper)\n",
			len(plan.Statements), o.path, len(r.targets), r.targetLabel)
	} else if wrap {
		fmt.Fprintf(r.out, "migration %s → %d shard(s) [%s] as one exec, role=%s statement_timeout=%s\n",
			o.path, len(r.targets), r.targetLabel, displayRole(r.role()), display(r.stmtTimeout))
	} else {
		fmt.Fprintf(r.out, "migration %s → %d shard(s) [%s] as one exec (pass-through)\n",
			o.path, len(r.targets), r.targetLabel)
	}
	if r.writeApprove {
		var confirmed bool
		if execution.AnyUnqualifiedWrite(content) {
			confirmed = r.confirmUnqualified()
		} else {
			confirmed = r.confirmWrite()
		}
		if !confirmed {
			fmt.Fprintln(r.out, "cancelled")
			return nil
		}
	}
	results, err := r.execWrite(content, wrap)
	if err != nil {
		// Отказ ДО выполнения (запрещённая операция, mixed-файл, нетранзакционная
		// миграция на prod без дедлайна, отмена и т.п.). Раньше этот путь молча
		// возвращал nil → headless/CI получали success-exit на не применённой
		// миграции. Теперь отказ — явная ошибка; объяснение уже напечатано внутри.
		return err
	}
	r.recordApplied(name, content, results)
	if plan.NonTransactional {
		r.warnPartialNonTransactional(results)
	} else {
		r.warnPartialApply(results)
	}
	return nil
}

// warnPartialApply предупреждает, что обёрнутая (транзакционная) миграция легла не на
// все шарды: успешные закоммитили её в своей транзакции, упавшие/отменённые — нет.
// Каждый шард атомарен сам по себе, но общей транзакции между шардами нет, поэтому
// кластер остаётся в СМЕШАННОМ состоянии (часть на новой версии, часть на старой).
func (r *REPL) warnPartialApply(results []db.ExecResult) {
	var ok, failed []string
	for _, res := range results {
		if res.Err != nil {
			failed = append(failed, res.Shard.Label)
		} else {
			ok = append(ok, res.Shard.Label)
		}
	}
	if len(ok) == 0 || len(failed) == 0 {
		return // всё применилось или всё упало — смешанного состояния нет
	}
	fmt.Fprintln(r.out, ui.Danger.Render(fmt.Sprintf("⚠ partial apply: %d shard(s) committed the migration, %d did NOT (%s)", len(ok), len(failed), strings.Join(failed, ", "))))
	fmt.Fprintln(r.out, "  the cluster is now on MIXED versions — finish with \\migrate --allowed --resume once the failure is understood.")
}

// warnPartialNonTransactional предупреждает, что нетранзакционная миграция НЕ
// атомарна: на шарде, упавшем посреди скрипта, остаются уже выполненные операторы,
// поэтому ledger не помечает его применённым, а наивный повтор проигрывает весь
// файл. Советует сделать операторы идемпотентными или вручную убрать частичное
// состояние перед повтором.
func (r *REPL) warnPartialNonTransactional(results []db.ExecResult) {
	var failed []string
	for _, res := range results {
		if res.Err != nil {
			failed = append(failed, res.Shard.Label)
		}
	}
	if len(failed) == 0 {
		return
	}
	fmt.Fprintln(r.out, ui.Danger.Render("⚠ non-transactional migration is NOT atomic — it failed mid-script on: "+strings.Join(failed, ", ")))
	fmt.Fprintln(r.out, "  statements that ran before the failure are already applied there; a re-run replays the whole file.")
	fmt.Fprintln(r.out, "  make statements idempotent (CREATE INDEX IF NOT EXISTS, DROP ... IF EXISTS) or clean up the partial state before retrying.")
}

// warnChecksumDrift предупреждает, если миграция с этим именем уже применялась в
// текущем контексте, но содержимое файла теперь ДРУГОЕ (sha256 не совпадает) —
// частый источник ошибок: отредактированный файл под тем же именем.
func (r *REPL) warnChecksumDrift(name, content string) {
	if r.applied == nil || name == "" {
		return
	}
	prev, ok := r.applied.Checksum(r.service+"/"+r.storage, name)
	if !ok {
		return
	}
	if prev != migration.Checksum(content) {
		fmt.Fprintln(r.out, ui.Danger.Render("⚠ migration "+name+" was previously applied here with DIFFERENT content (checksum mismatch)"))
		fmt.Fprintln(r.out, "  the file under this name has changed since it was last applied — re-applying may double-apply or conflict.")
		fmt.Fprintln(r.out, "  use a new migration name for the change, or pass --force to override.")
	}
}

// recordApplied записывает в ledger каждый шард, где миграция прошла успешно,
// и фиксирует контрольную сумму содержимого для последующего детекта дрейфа.
func (r *REPL) recordApplied(name string, content string, results []db.ExecResult) {
	if name == "" || len(results) == 0 {
		return
	}
	ctxKey := r.service + "/" + r.storage
	ts := r.now()
	anyOK := false
	for _, res := range results {
		if res.Err == nil {
			anyOK = true
			// Миграция уже закоммичена на этом шарде; ошибку сохранения локального
			// ledger нельзя глотать — пропущенная запись делает миграцию
			// «незавершённой» и провоцирует повтор, меняющий данные. Сообщаем громко.
			if err := r.applied.Record(ctxKey, name, res.Shard.Label, ts); err != nil {
				fmt.Fprintln(r.out, ui.Danger.Render(fmt.Sprintf(
					"⚠ migration applied on %s but the local ledger could not be saved: %v", res.Shard.Label, oneLine(err.Error()))))
				fmt.Fprintln(r.out, "  --resume will under-report this shard — do NOT blindly re-run; verify state before retrying.")
			}
		}
	}
	// Фиксируем контрольную сумму содержимого (для warnChecksumDrift при повторном
	// применении), если миграция прошла хотя бы на одном шарде.
	if anyOK {
		if err := r.applied.RecordChecksum(ctxKey, name, migration.Checksum(content)); err != nil {
			fmt.Fprintf(r.out, "note: could not save migration checksum: %v\n", oneLine(err.Error()))
		}
	}
}

// previewMigration печатает точный SQL, который отправит terox, для dry-run.
func (r *REPL) previewMigration(content string, wrap bool, plan migration.Plan) {
	if plan.Mixed {
		fmt.Fprintln(r.out, "-- MIXED FILE: would be REJECTED --")
		fmt.Fprintln(r.out, "-- split CONCURRENTLY/VACUUM statements into a separate file --")
		return
	}
	if wrap && migration.HasTxControl(content) {
		fmt.Fprintln(r.out, "-- REFUSED: carries its own BEGIN/COMMIT/ROLLBACK or SET ROLE --")
		fmt.Fprintln(r.out, "-- would bypass the protective wrapper; remove tx-control, or use \\i to run verbatim --")
		return
	}
	// Та же проверка session-state firewall, что и на пути применения, чтобы
	// dry-run не показывал «обёрнутый payload», который реальный --allowed отклонит.
	if wrap {
		if reason := migration.SessionStateViolation(content); reason != "" {
			fmt.Fprintln(r.out, "-- REFUSED: "+strings.TrimPrefix(reason, "refused: ")+" --")
			return
		}
	}
	if plan.NonTransactional {
		fmt.Fprintln(r.out, "-- non-transactional: each statement runs as its own exec --")
		for i, s := range plan.Statements {
			fmt.Fprintf(r.out, "-- [%d/%d] --\n%s;\n", i+1, len(plan.Statements), s)
		}
		return
	}
	payload := content
	if wrap {
		built, err := migration.BuildTransactional(content, r.role(), r.stmtTimeout, r.cfg.LockTimeout)
		if err != nil {
			fmt.Fprintf(r.out, "-- REFUSED: %v --\n", err)
			return
		}
		payload = built
	}
	fmt.Fprintln(r.out, "-- exact exec terox sends to each shard --")
	fmt.Fprint(r.out, payload)
	if !strings.HasSuffix(payload, "\n") {
		fmt.Fprintln(r.out)
	}
}

// setTimeout меняет statement_timeout сессии (показан в строке статуса).
func (r *REPL) setTimeout(args []string) error {
	var choice string
	if len(args) == 0 {
		opts := []string{"500ms", "1s", "5s", "30s", "60s", "5min", "off", "custom"}
		c, aborted, err := huhSelect("statement_timeout", opts)
		if err != nil {
			return err
		}
		if aborted {
			return nil // отмена
		}
		if c == "custom" {
			c = strings.TrimSpace(r.readLine("statement_timeout (e.g. 500ms, 2min): "))
		}
		choice = c
	} else {
		choice = strings.TrimSpace(args[0])
	}
	if choice == "off" {
		choice = ""
	}
	v := normalizeTimeout(choice)
	// Отвергаем всё, что не принял бы PostgreSQL: иначе неверное значение попадёт
	// в SET statement_timeout и сломает все дальнейшие чтения и миграции без
	// понятной причины.
	if v != "" && !pgTimeoutRe.MatchString(v) {
		return fmt.Errorf("invalid statement_timeout %q — use e.g. 500ms, 2s, 5min, or off", choice)
	}
	r.stmtTimeout = v
	fmt.Fprintf(r.out, "statement_timeout = %s\n", display(r.stmtTimeout))
	return nil
}

var (
	bareIntRe = regexp.MustCompile(`^[0-9]+$`)
	// pgTimeoutRe — грамматика statement_timeout PostgreSQL: "0" или число
	// (целое/дробное) с опциональной единицей us/ms/s/min/h/d.
	pgTimeoutRe = regexp.MustCompile(`(?i)^(0|[0-9]+(\.[0-9]+)?\s?(us|ms|s|min|h|d)?)$`)
)

// normalizeTimeout превращает голое целое в явные миллисекунды (единица по
// умолчанию в PostgreSQL для таймаута без единицы), чтобы значение было однозначным.
func normalizeTimeout(v string) string {
	if bareIntRe.MatchString(v) {
		return v + "ms"
	}
	return v
}

func display(s string) string {
	if s == "" {
		return "off"
	}
	return s
}

func displayRole(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func contextWithOptionalTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return ctx, func() {}
}

// editInEditor открывает $VISUAL (приоритетно) или $EDITOR с временным файлом,
// предварительно заполненным seed (например, последним запросом — чтобы большой
// запрос можно было править и перезапускать как в psql `\e`), и возвращает итоговое
// содержимое. Переменная редактора разбивается по пробелам, чтобы работала команда
// с аргументами (например, EDITOR="code --wait"); запасной — vi.
func (r *REPL) editInEditor(seed string) (string, error) {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		parts = []string{"vi"}
	}
	f, err := os.CreateTemp("", "terox-*.sql")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if seed != "" {
		if _, err := f.WriteString(seed); err != nil {
			f.Close()
			os.Remove(name)
			return "", err
		}
	}
	f.Close()
	defer os.Remove(name)

	cmd := exec.Command(parts[0], append(parts[1:], name)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	data, err := os.ReadFile(name)
	return string(data), err
}

func (r *REPL) printList() {
	for _, svc := range r.cfg.ServiceNames() {
		fmt.Fprintf(r.out, "%s\n", svc)
		for _, sto := range r.cfg.StorageNames(svc) {
			st := r.cfg.Services[svc].Storages[sto]
			if st == nil {
				fmt.Fprintf(r.out, "   %-16s (empty)\n", sto)
				continue
			}
			count := st.Count
			if count <= 0 {
				count = 1
			}
			marker := " "
			if svc == r.service && sto == r.storage {
				marker = "*"
			}
			fmt.Fprintf(r.out, " %s %-16s %d shard(s)  %s:%d\n", marker, sto, count, st.HostTemplate, st.Port)
		}
	}
}

func (r *REPL) printShards() {
	active := map[int]bool{}
	for _, t := range r.targets {
		active[t.Position] = true
	}
	for _, s := range r.shards {
		marker := " "
		if active[s.Position] {
			marker = "*"
		}
		fmt.Fprintf(r.out, " %s %-8s %s:%d/%s\n", marker, s.Label, s.Host, s.Port, s.DB)
	}
	fmt.Fprintf(r.out, "(targeting %d/%d shards: %s)\n", len(r.targets), len(r.shards), r.targetLabel)
}

// tokenizeArgs разбивает строку мета-команды на токены с учётом одинарных и
// двойных кавычек (в двойных обрабатываются \-экранирования), чтобы аргумент-путь
// мог содержать пробелы: \migrate "/path with space/m.sql". Ведущая \команда
// сохраняет обратный слэш (вне кавычек он литерал).
func tokenizeArgs(s string) []string {
	var toks []string
	var cur strings.Builder
	inTok := false
	flush := func() {
		if inTok {
			toks = append(toks, cur.String())
			cur.Reset()
			inTok = false
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case ' ', '\t':
			flush()
			i++
		case '\'':
			inTok = true
			i++
			for i < len(s) && s[i] != '\'' {
				cur.WriteByte(s[i])
				i++
			}
			if i < len(s) {
				i++ // закрывающая кавычка
			}
		case '"':
			inTok = true
			i++
			for i < len(s) {
				if s[i] == '\\' && i+1 < len(s) {
					cur.WriteByte(s[i+1])
					i += 2
					continue
				}
				if s[i] == '"' {
					i++
					break
				}
				cur.WriteByte(s[i])
				i++
			}
		default:
			inTok = true
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return toks
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// parseOnOff строго разбирает аргумент-переключатель on/off: без аргумента
// инвертирует cur, известное слово on/off задаёт значение, прочее — ошибка
// (чтобы опечатка молча не выключила настройку).
func parseOnOff(args []string, cur bool) (bool, error) {
	if len(args) == 0 {
		return !cur, nil
	}
	if len(args) > 1 {
		return cur, fmt.Errorf("expected a single on|off argument, got %d", len(args))
	}
	switch strings.ToLower(args[0]) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return cur, fmt.Errorf("expected on or off, got %q", args[0])
}

// parseMaxRows разбирает аргумент \maxrows: неотрицательное целое или
// "unlimited"/"all"/"0" для снятия лимита. Неверный или отрицательный ввод —
// ошибка.
func parseMaxRows(s string) (int, error) {
	switch strings.ToLower(s) {
	case "unlimited", "all", "0":
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("\\maxrows: expected a non-negative integer or 'unlimited', got %q", s)
	}
	return n, nil
}

func maxRowsLabel(n int) string {
	if n <= 0 {
		return "unlimited"
	}
	return strconv.Itoa(n)
}

const listTablesSQL = `SELECT schemaname AS schema, relname AS table, n_live_tup AS est_rows
FROM pg_stat_user_tables ORDER BY schemaname, relname`

// listSchemasSQL — пользовательские схемы (без системных pg_*/information_schema).
const listSchemasSQL = `SELECT nspname AS schema
FROM pg_namespace
WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
ORDER BY nspname`

// listIndexesSQL — индексы пользовательских таблиц с их размером.
const listIndexesSQL = `SELECT schemaname AS schema, tablename AS table, indexname AS index,
       pg_size_pretty(pg_relation_size((quote_ident(schemaname) || '.' || quote_ident(indexname))::regclass)) AS size
FROM pg_indexes
WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
ORDER BY schemaname, tablename, indexname`

// describeTableSQL разрешает целевую таблицу через to_regclass(%s), где %s —
// [schema.]name в кавычках как строковый литерал. Для голого имени to_regclass
// применяет серверный search_path (так `\d users` возвращает ровно ОДНУ видимую
// таблицу, а не смесь public.users + items.users) и учитывает явный префикс схемы
// (так работает `\d items.users`). При отсутствии совпадения возвращает NULL, а
// запрос — ноль строк.
// "column"/"default" — зарезервированные слова, поэтому псевдонимы в двойных
// кавычках: SQL остаётся валидным, а заголовки колонок — читаемыми.
const describeTableSQL = `SELECT a.attname AS "column",
    format_type(a.atttypid, a.atttypmod) AS "type",
    CASE WHEN a.attnotnull THEN 'no' ELSE 'yes' END AS "nullable",
    pg_get_expr(d.adbin, d.adrelid) AS "default"
FROM pg_attribute a
LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE a.attrelid = to_regclass(%s) AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`
