// Package repl — интерактивный psql-подобный цикл: выбор контекста,
// многострочный ввод, мета-команды, защита записи и вывод результатов.
package repl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/huh"
	"github.com/chzyer/readline"

	"terox/internal/cluster"
	"terox/internal/complete"
	"terox/internal/config"
	"terox/internal/db"
	"terox/internal/execution"
	"terox/internal/migration"
	"terox/internal/render"
	"terox/internal/sqlsplit"
	"terox/internal/store"
	"terox/internal/ui"
	"terox/internal/wizard"
)

// REPL хранит состояние сессии интерактивного цикла.
type REPL struct {
	cfg  *config.Config
	mgr  *db.Manager
	rl   *readline.Instance
	comp *completer // общий для painter readline и редактора bubbletea
	out  io.Writer

	// useTeaEditor выбирает строчный редактор bubbletea (живое меню дополнения)
	// вместо readline. tea — редактор по умолчанию; readline включают через
	// TEROX_EDITOR=readline, ключ editor: в конфиге или команду \editor.
	useTeaEditor bool
	// history — история строк в памяти для редактора bubbletea (заполняется из
	// файла истории readline; пополняется при отправке).
	history  []string
	histPath string // файл истории readline, читается также tea-редактором
	// historyOff отключает запись истории на эту сессию (\history off): ни диск,
	// ни память не пополняются, пока не включат обратно (\history on).
	historyOff bool

	service     string
	storage     string
	shards      []cluster.Shard // все шарды текущего хранилища
	targets     []cluster.Shard // активное подмножество, где выполняются запросы
	targetLabel string          // "all", "rs005" или "0-2,5"

	writeMode bool
	timing    bool
	maxRows   int

	// stmtTimeout — текущий statement_timeout для защиты миграций; берётся из
	// конфига, переопределяется через \timeout. Всегда виден в строке статуса.
	stmtTimeout string
	// prod — флаг прод-окружения текущего хранилища (управляет бейджем prod в
	// приглашении и дополнительными подтверждениями/предупреждениями при записи,
	// НЕ ролью записи).
	prod bool
	// migrationRole — настроенная роль записи текущего хранилища
	// (Storage.MigrationRole). Пусто — хранилище берёт роль из общего значения
	// кластера или не использует её; см. role().
	migrationRole string
	// expanded переключает вертикальный вывод (запись на строку) для широких строк.
	expanded bool
	// impact управляет предпросмотром числа затрагиваемых строк перед записью (по умолчанию выкл).
	impact bool
	// writeApprove управляет запросом подтверждения перед записью (по умолчанию вкл;
	// отключается командой \write_approve off).
	writeApprove bool
	// suggest управляет встроенной призрачной подсказкой (по умолчанию вкл).
	suggest bool
	// showSystemCatalog включает объекты pg_catalog/information_schema в
	// дополнение (по умолчанию выкл — предлагаются только объекты своей БД).
	showSystemCatalog bool
	// catalog — типизированный снимок для дополнения (схемы/отношения/колонки/
	// функции, search_path, зарезервированные ключевые слова, покрытие шардов).
	// Строится в ФОНЕ (чтобы первый Tab не ждал тяжёлый запрос каталога) и
	// очищается при смене контекста. Все поля каталога защищены catalogMu, так
	// как загрузчик работает в своей горутине. catalogEpoch увеличивается при
	// каждой смене контекста, чтобы отбросить устаревшую загрузку; catalogAttempt
	// управляет паузой повторов; catalogErr хранит последнюю ошибку для
	// \completion status.
	catalogMu      sync.Mutex
	catalog        *complete.Catalog
	catalogLoading bool
	catalogEpoch   int
	catalogAttempt time.Time
	catalogErr     error
	// catalogNotice — одноразовая заметка о деградации каталога (partial/forbidden/
	// timeout сегменты), выставляется фоновым загрузчиком и печатается основным
	// циклом перед следующим приглашением (P2-5: degraded-состояние surface'ится
	// проактивно, а не только по \completion status). Под catalogMu.
	catalogNotice string
	// colFetching устраняет дублирование фоновых загрузок колонок по отношению
	// (ленивая загрузка), ключ "schema\x00relation". Защищено catalogMu.
	colFetching map[string]bool
	// pkCache кэширует имена колонок первичного ключа отношения (для сокращения
	// "table.pkey=value" в \find/\count и его дополнения), ключ
	// "schema\x00relation" (схема "" = разрешается через search_path). pkFetching
	// устраняет дублирование фоновых загрузок. Оба под catalogMu; очищаются при
	// смене контекста.
	pkCache    map[string][]string
	pkFetching map[string]bool
	// serverVer кэширует server_version_num текущего хранилища (0 = неизвестно),
	// чтобы анализатор плана учитывал версию в правилах. Сбрасывается при смене контекста.
	serverVer int

	// lastQuery / lastCols / lastRows хранят последний прочитанный результат для
	// \export и \save. lastTruncated отмечает, что строки в памяти были урезаны
	// (на сервере есть ещё), поэтому \export перечитывает полный результат, а не
	// усечённый срез. lastTargets хранит физическое подмножество шардов, откуда
	// пришёл результат, чтобы усечённый \export перечитывал те же самые шарды,
	// даже если контекст сменился.
	lastQuery     string
	lastCols      []string
	lastRows      [][]any
	lastTruncated bool
	lastTargets   []cluster.Shard

	queries *store.Queries // сохранённые именованные запросы
	applied *store.Applied // журнал применённых миграций по шардам
	now     func() string  // источник меток времени (подменяется в тестах)

	// lastWorkload — последний снимок нагрузки (pg_stat_statements, агрегат по
	// шардам) для трендов: \statements snapshot захватывает, \statements diff
	// сравнивает с текущим (F9+). queryid server-local — снапшот валиден в пределах
	// сервера/мажора.
	lastWorkload *workloadSnapshot

	startupTarget string // "service/storage/selector", пропускает выбор при задании

	// healthMsg — краткая заметка о доступности для строки статуса ("" когда
	// текущие цели доступны или ещё не проверены). healthStale отмечает, что цели
	// сменились и перед следующим приглашением нужен свежий ping, чтобы
	// недоступная БД показалась сразу, а не на первом запросе.
	healthMsg   string
	healthStale bool
}

// SetStartupTarget заранее выбирает контекст ("service/storage/selector"),
// чтобы Run пропустил интерактивный выбор.
func (r *REPL) SetStartupTarget(t string) { r.startupTarget = t }

// New создаёт REPL, привязанный к cfg и новому менеджеру пулов.
func New(cfg *config.Config) (*REPL, error) {
	histPath, err := historyPath()
	if err != nil {
		return nil, err
	}
	rl, err := readline.NewEx(&readline.Config{
		HistoryFile:       histPath,
		HistorySearchFold: true,
		// КРИТИЧНО: отключаем авто-сохранение readline. Иначе библиотека пишет
		// КАЖДУЮ введённую строку (включая незавершённые многострочные фрагменты,
		// ответы на подтверждения и строки с паролем) и в файл, и в кольцо Ctrl-R,
		// в обход recordHistory и его проверок isSensitiveStatement/historyOff.
		// При выключенном авто-сохранении единственный, кто пишет историю, —
		// recordHistory (только завершённые не-секретные операторы).
		DisableAutoSaveHistory: true,
		InterruptPrompt:        "^C",
		EOFPrompt:              "\\q",
	})
	if err != nil {
		return nil, err
	}
	queries, err := store.LoadQueries()
	if err != nil {
		return nil, err
	}
	applied, err := store.LoadApplied()
	if err != nil {
		return nil, err
	}

	r := &REPL{
		cfg:          cfg,
		mgr:          db.NewManager(),
		rl:           rl,
		out:          os.Stdout,
		writeMode:    cfg.WriteModeDefault,
		maxRows:      cfg.MaxRowsValue(),
		stmtTimeout:  cfg.StatementTimeout,
		timing:       cfg.TimingEnabled(),
		impact:       cfg.ImpactEnabled(),
		suggest:      cfg.SuggestEnabled(),
		expanded:     cfg.ExpandedDefault(),
		writeApprove: cfg.WriteApproveEnabled(),
		queries:      queries,
		applied:      applied,
		now:          func() string { return time.Now().Format("2006-01-02 15:04:05") },
	}
	comp := newCompleter(r)
	r.comp = comp
	rl.Config.AutoComplete = comp
	rl.Config.Painter = &painter{r: r, comp: comp}
	r.histPath = histPath
	// Выбор редактора: TEROX_EDITOR важнее конфига, конфиг важнее значения по
	// умолчанию. По умолчанию — редактор "tea" с живым дополнением; для
	// классического выбирают "readline" (env, конфиг или \editor).
	choice := os.Getenv("TEROX_EDITOR")
	if choice == "" {
		choice = cfg.Editor
	}
	if choice == "" {
		choice = "tea"
	}
	if choice == "tea" {
		r.useTeaEditor = true
		r.history = loadHistoryLines(histPath)
	}
	return r, nil
}

// loadHistoryLines читает файл истории readline в срез (по возможности),
// чтобы редактор bubbletea мог листать его стрелками Вверх/Вниз.
func loadHistoryLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

// Close освобождает ресурсы.
func (r *REPL) Close() {
	r.mgr.Close()
	_ = r.rl.Close()
}

// Run запускает интерактивный цикл.
func (r *REPL) Run() error {
	defer r.Close()

	fmt.Fprintf(r.out, "%s  %d service(s): %s\n",
		ui.Dim.Render("config: "+r.cfg.Path()),
		len(r.cfg.Services), strings.Join(r.cfg.ServiceNames(), ", "))

	if r.cfg.IsEmpty() {
		fmt.Fprintln(r.out, "No clusters registered yet. Let's add one (\\add anytime).")
		if _, _, err := wizard.Run(r.cfg); err != nil {
			fmt.Fprintf(r.out, "registration cancelled: %v\n", err)
			return nil
		}
	}

	if r.startupTarget != "" {
		if err := r.applyTarget(r.startupTarget); err != nil {
			return err
		}
	} else if err := r.selectContext(""); err != nil {
		if errors.Is(err, errSelectQuit) {
			return nil // пользователь вышел из верхнего меню
		}
		return err
	}

	var buf strings.Builder
	for {
		if buf.Len() == 0 && r.healthStale {
			r.checkHealth()
			r.healthStale = false
			if r.healthMsg != "" {
				fmt.Fprintln(r.out, ui.Danger.Render("⚠ "+r.healthMsg+" — check connectivity (\\ping for details)"))
			}
		}
		if buf.Len() == 0 {
			r.kickCatalog() // прогреть каталог дополнения в фоне
			// Проактивно показываем заметку о деградации каталога один раз после
			// фоновой загрузки (P2-5): пользователь сразу видит partial-дополнение,
			// не запуская \completion status.
			if notice := r.takeCatalogNotice(); notice != "" {
				fmt.Fprintln(r.out, ui.Dim.Render(notice))
			}
		}
		line, err := r.readInputLine(buf.Len() > 0)
		if err == readline.ErrInterrupt {
			buf.Reset()
			continue
		}
		if err == io.EOF {
			fmt.Fprintln(r.out)
			return nil
		}
		if err != nil {
			return err
		}

		trimmed := strings.TrimSpace(line)

		// Мета-команды распознаются только в начале оператора.
		if buf.Len() == 0 && strings.HasPrefix(trimmed, "\\") {
			// Завершённую мета-команду кладём в историю (как и SQL-оператор), чтобы
			// её можно было листать ↑/Ctrl-R. recordHistory отфильтрует пустое и
			// уважает \history off; секрета в мета-командах нет.
			r.recordHistory(trimmed)
			quit, err := r.runMeta(trimmed)
			if err != nil {
				fmt.Fprintf(r.out, "error: %v\n", err)
			}
			if quit {
				return nil
			}
			continue
		}

		if trimmed == "" && buf.Len() == 0 {
			continue
		}

		buf.WriteString(line)
		buf.WriteByte('\n')

		// Выполнять, когда накопленный буфер заканчивается настоящей точкой с
		// запятой — сначала маскируем литералы/комментарии, чтобы ";" внутри
		// строки или комментария не разрывал многострочный оператор раньше времени.
		if strings.HasSuffix(strings.TrimSpace(sqlsplit.Mask(buf.String())), ";") {
			sql := strings.TrimSpace(buf.String())
			buf.Reset()
			r.recordHistory(sql) // только завершённый не-секретный оператор → диск и память
			r.runStatement(strings.TrimSuffix(sql, ";"))
		}
	}
}

// readInputLine читает одну строку ввода: через редактор bubbletea (живое меню
// дополнения), если он включён, иначе через readline. Маркеры tea-редактора
// переводятся в маркеры readline, чтобы окружающий цикл не менялся.
func (r *REPL) readInputLine(continuation bool) (string, error) {
	prompt := r.prompt(continuation)
	if !r.useTeaEditor {
		r.rl.SetPrompt(prompt)
		return r.rl.Readline()
	}
	line, err := r.readLineTea(prompt)
	switch err {
	case errEditorInterrupt:
		return "", readline.ErrInterrupt
	case errEditorEOF:
		return "", io.EOF
	case nil:
		// История в памяти пополняется только ЗАВЕРШЁННЫМ оператором через
		// recordHistory на месте выполнения — здесь отдельные строки (в т.ч.
		// незавершённые многострочные и содержащие пароль) НЕ сохраняются, иначе
		// секрет можно было бы достать через Ctrl-R в этой же сессии.
		return line, nil
	default:
		return "", err
	}
}

// prompt строит цветное приглашение: зелёный контекст (базы), красные опасные
// режимы (write/prod), янтарный statement_timeout — строка статуса.
func (r *REPL) prompt(continuation bool) string {
	plainHead := r.contextLabel() + " " + r.statusPlain()
	if continuation {
		// ".. " — 3 видимых символа под 3-символьную стрелку "=> ", чтобы
		// продолжение ввода выравнивалось под первой строкой.
		return strings.Repeat(" ", len(plainHead)+1) + ui.Dim.Render(".. ")
	}
	coloredHead := ui.Service.Render(fmt.Sprintf("%s/%s", r.service, r.storage)) +
		ui.Shard.Render("("+r.targetDisplay()+")") + " " + r.statusColored()
	arrow := " " + ui.Arrow.Render("=> ")
	if r.writeMode {
		arrow = " " + ui.Danger.Render("=> ")
	}
	return coloredHead + arrow
}

func (r *REPL) contextLabel() string {
	return fmt.Sprintf("%s/%s(%s)", r.service, r.storage, r.targetDisplay())
}

// targetDisplay — часть приглашения с шардом: при одном выбранном шарде
// показывает "label · db" (чтобы текущая база была видна), если метка не равна
// базе. Для нескольких шардов показывает селектор.
func (r *REPL) targetDisplay() string {
	if len(r.targets) == 1 {
		return labelWithDB(r.targets[0])
	}
	return r.targetLabel
}

// labelWithDB рисует "label/db", когда имя базы отличается от метки (чтобы
// текущая база была видна), иначе только метку. Общая для приглашения и
// интерактивного выбора шарда.
func labelWithDB(s cluster.Shard) string { return s.LabelDB() }

// statusPlain рисует статус без цвета (для расчёта ширины приглашения).
func (r *REPL) statusPlain() string {
	env, mode := "staging", "ro"
	if r.prod {
		env = "prod"
	}
	if r.writeMode {
		mode = "wr"
	}
	st := r.stmtTimeout
	if st == "" {
		st = "off"
	}
	return fmt.Sprintf("[%s %s st=%s]%s", env, mode, st, healthSuffixPlain(r.healthMsg))
}

// healthSuffixPlain рисует заметку о доступности (без цвета) для ширины приглашения.
func healthSuffixPlain(msg string) string {
	if msg == "" {
		return ""
	}
	return " (" + msg + ")"
}

// statusColored рисует всегда видимый статус: окружение, режим чтения/записи и
// statement_timeout, опасные состояния — красным.
func (r *REPL) statusColored() string {
	env := ui.Safe.Render("staging")
	if r.prod {
		env = ui.Danger.Render("prod")
	}
	mode := ui.Safe.Render("ro")
	if r.writeMode {
		mode = ui.Danger.Render("wr")
	}
	st := r.stmtTimeout
	if st == "" {
		st = "off"
	}
	status := ui.Dim.Render("[") + env + " " + mode + " " +
		ui.Dim.Render("st=") + ui.Timeout.Render(st) + ui.Dim.Render("]")
	if r.healthMsg != "" {
		status += ui.Danger.Render(" (" + r.healthMsg + ")")
	}
	return status
}

// checkHealth пингует текущие цели и записывает краткую заметку о доступности
// для строки статуса. Выполняется один раз при смене контекста (вход в консоль
// или переключение шардов), чтобы недоступная БД показалась сразу, а не только
// при таймауте первого запроса.
//
// Каждый шард получает СВОЙ таймаут, а не общий настенный дедлайн. При
// ограниченном пуле воркеров общий дедлайн ущемляет шарды из поздней волны: пока
// освобождается воркер, большая часть бюджета уже потрачена, и хвост ложно
// помечается недоступным — особенно на больших кластерах и когда TLS-рукопожатие
// (sslmode != disable) замедляет каждое холодное подключение. Поэтому проверка с
// общим дедлайном может расходиться с \ping (который использует полный таймаут на
// шард и прогретые пулы).
func (r *REPL) checkHealth() {
	if len(r.targets) == 0 {
		r.healthMsg = ""
		return
	}
	// Два прохода: короткая проба, затем повтор ТОЛЬКО для отставших с щедрым
	// бюджетом, прежде чем что-то объявить недоступным. Самый первый контакт с
	// шардом — это ХОЛОДНОЕ подключение (TCP + TLS-рукопожатие + аутентификация) и
	// может занять несколько секунд на удалённом проде, так что одна короткая проба
	// ложно помечает работающие шарды. Шард считается упавшим, только если он
	// провалил И быструю пробу, И медленный повтор.
	failed := r.pingFailures(r.targets, 3*time.Second)
	if len(failed) > 0 {
		retry := time.Duration(r.cfg.QueryTimeout)
		if retry < 10*time.Second {
			retry = 10 * time.Second // запас на медленное холодное TLS-рукопожатие
		}
		failed = r.pingFailures(failed, retry)
	}
	switch down := len(failed); {
	case down == 0:
		r.healthMsg = ""
	case len(r.targets) == 1:
		r.healthMsg = "unreachable"
	default:
		r.healthMsg = fmt.Sprintf("%d/%d unreachable", down, len(r.targets))
	}
}

// pingFailures пингует шарды параллельно (с ограничением fanout_concurrency),
// каждый со СВОИМ таймаутом (чтобы поздняя волна не страдала от уже потраченного
// времени), и возвращает шарды, которые не ответили.
func (r *REPL) pingFailures(shards []cluster.Shard, perShardTimeout time.Duration) []cluster.Shard {
	conc := r.cfg.ProbeConcurrency(len(shards))
	if conc <= 0 {
		conc = 16
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed []cluster.Shard
	for _, s := range shards {
		wg.Add(1)
		sem <- struct{}{} // слот берём ДО запуска: иначе на N шардов разом стартует N горутин
		go func(s cluster.Shard) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), perShardTimeout)
			defer cancel()
			if err := r.mgr.Ping(ctx, s); err != nil {
				mu.Lock()
				failed = append(failed, s)
				mu.Unlock()
			}
		}(s)
	}
	wg.Wait()
	return failed
}

// runStatement классифицирует, проверяет и выполняет один введённый оператор.
// Чтения выводятся таблицами; записи идут защищённым путём (set role +
// statement_timeout, один exec) и выводят статус по шардам.
func (r *REPL) runStatement(sql string) {
	if strings.TrimSpace(sql) == "" {
		return
	}
	if len(r.targets) == 0 {
		fmt.Fprintln(r.out, "no shards selected")
		return
	}
	r.lastQuery = strings.TrimSpace(sql)
	// Сразу очищаем экспортируемый результат; его заново заполняет только
	// полностью успешный SELECT ниже, так что \export не запишет устаревший или
	// ошибочный результат.
	r.clearLastResult()

	// Единый ExecutionPlanner владеет всем решением read-vs-write / refuse /
	// confirm (аудит 4.1): UI не решает сам, а подчиняется плану. Порядок проверок
	// (режим записи → tx-control → session-state → подтверждение) живёт в плане.
	plan := (execution.Planner{}).Plan(execution.Request{SQL: sql, WriteMode: r.writeMode})
	if plan.Refused() {
		switch plan.Refusal.Code {
		case execution.RefuseTxControl:
			r.refuseTxControl()
		case execution.RefuseSessionState:
			r.refuseSessionState(plan.Refusal.Message)
		default:
			fmt.Fprintln(r.out, plan.Refusal.Message)
		}
		return
	}
	if !plan.IsWrite {
		r.execRead(sql)
		return
	}

	// Введённая запись оборачивается защитной конструкцией (set local role +
	// statement_timeout + lock_timeout). Показать, сколько строк затронет
	// UPDATE/DELETE на каждом шарде — безопаснее, чем psql "выстрелил и смотри";
	// предпросмотр по умолчанию выключен и включается через \impact on, запрос
	// подтверждения можно отключить через \write_approve off.
	countsKnown := r.previewImpact(sql)
	if r.writeApprove {
		// План выносит предупреждения (особенно неочевидный случай волатильной
		// функции с побочным эффектом в SELECT, который read-only транзакция не
		// блокирует — P1-3).
		for _, w := range plan.Warnings {
			fmt.Fprintln(r.out, ui.Danger.Render("⚠ "+w))
		}
		var ok bool
		if plan.Confirm == execution.ConfirmUnqualified {
			ok = r.confirmUnqualified() // строгий барьер для без-WHERE / TRUNCATE
		} else {
			ok = r.confirmWrite()
		}
		if !ok {
			fmt.Fprintln(r.out, "cancelled")
			return
		}
		// Если предпросмотр не смог посчитать каждый шард, радиус поражения
		// неизвестен — требуем явное дополнительное подтверждение.
		if !countsKnown {
			fmt.Fprintln(r.out, ui.Danger.Render("⚠ предпросмотр не сработал на части шардов ('?') — число затрагиваемых строк неизвестно"))
			if strings.TrimSpace(r.readLine("введите 'yes', чтобы продолжить при неизвестном эффекте: ")) != "yes" {
				fmt.Fprintln(r.out, "cancelled (unknown impact)")
				return
			}
		}
	}
	// Введённая запись — это тело: оборачиваем его защитной конструкцией.
	r.execWrite(sql, true)
}

// refuseTxControl объясняет, почему оборачиваемая запись/миграция со своим
// управлением транзакцией или сменой роли отклоняется, а не выполняется молча
// без защитной конструкции.
func (r *REPL) refuseTxControl() {
	fmt.Fprintln(r.out, "refused: this statement carries its own BEGIN/COMMIT/ROLLBACK or SET ROLE,")
	fmt.Fprintln(r.out, "which would bypass terox's protective wrapper (set local role + statement_timeout + lock_timeout).")
	fmt.Fprintln(r.out, "remove the transaction control so terox can wrap it, or use \\i <file> to run a self-contained script verbatim (you own the role/timeouts).")
}

// refuseSessionState объясняет, почему оборачиваемая запись/миграция с
// session-scoped конструкцией (SET search_path, TEMP, LISTEN, PREPARE, cursor,
// session advisory lock, DISCARD) отклоняется: такое состояние переживает COMMIT
// обёртки и при transaction pooling утекает следующему клиенту. reason уже
// называет конструкцию, причину и безопасную альтернативу.
func (r *REPL) refuseSessionState(reason string) {
	fmt.Fprintln(r.out, reason)
}

// refuseForbidden объясняет, почему операция отклонена БЕЗУСЛОВНО (например DROP
// DATABASE): для неё нет безопасного сценария из terox, и её нельзя включить ни
// write-режимом, ни подтверждением.
func (r *REPL) refuseForbidden(op string) {
	fmt.Fprintf(r.out, "refused: %s is permanently disabled in terox.\n", op)
	fmt.Fprintln(r.out, "  it irreversibly destroys an entire shard database and has no safe path from this client —")
	fmt.Fprintln(r.out, "  if you truly intend it, do it out-of-band with psql/admin tooling.")
}

// pgDurationToGo переводит длительность PostgreSQL ("300ms", "5s", "2min" или
// голое число = миллисекунды) в длительность Go.
func pgDurationToGo(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" || s == "off" {
		return 0, false
	}
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	val, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, false
	}
	var mult time.Duration
	switch strings.TrimSpace(s[i:]) {
	case "us":
		mult = time.Microsecond
	case "ms", "":
		mult = time.Millisecond
	case "s":
		mult = time.Second
	case "min":
		mult = time.Minute
	case "h":
		mult = time.Hour
	case "d":
		mult = 24 * time.Hour
	default:
		return 0, false
	}
	return time.Duration(val * float64(mult)), true
}

// execRead выполняет запрос чтения (всегда внутри read-only транзакции).
// Сессионный statement_timeout (виден в приглашении) ограничивает чтения на
// сервере; он действует только на этот вызов, чтобы диагностические чтения
// (\doctor, дополнение) не затрагивались. Клиентский query_timeout — лишь страховка
// от зависания и не должен быть меньше statement_timeout, иначе перехватит его.
func (r *REPL) execRead(sql string) {
	r.execReadCtx(context.Background(), sql)
}

// execReadCtx выполняет чтение под переданным родительским контекстом, чтобы
// интерактивный драйвер (\watch) мог отменить выполняющийся запрос по Ctrl-C.
// Путь одной цели пробрасывает контекст в запрос; путь нескольких целей идёт
// через fanoutLimit, который ставит свою отмену по SIGINT.
func (r *REPL) execReadCtx(parent context.Context, sql string) {
	// Очищаем экспортируемый результат и здесь (не только в runStatement), чтобы
	// вызывающие, попадающие в execRead напрямую — \watch — тоже сбрасывали
	// прошлый результат; тогда неудачное чтение не оставляет устаревшего.
	r.clearLastResult()
	r.mgr.SetReadTimeout(r.stmtTimeout)
	defer r.mgr.SetReadTimeout("")
	ctx := parent
	timeout := time.Duration(r.cfg.QueryTimeout)
	if st, ok := pgDurationToGo(r.stmtTimeout); ok && st+2*time.Second > timeout {
		timeout = st + 2*time.Second // дать серверному statement_timeout сработать первым
	}
	// Ограничиваем число материализуемых строк для интерактивного чтения пределом
	// отображения (maxRows); рендерер всё равно больше не покажет, а \export
	// перечитывает полный результат при усечении. maxRows == 0 (без предела) —
	// явный выбор пользователя без ограничения.
	readLimit := r.maxRows
	if len(r.targets) == 1 {
		cctx, cancel := contextWithOptionalTimeout(ctx, timeout)
		defer cancel()
		res, err := r.mgr.ExecLimit(cctx, r.targets[0], sql, true, readLimit)
		if err != nil {
			fmt.Fprintf(r.out, "error: %v\n", oneLine(err.Error()))
			return
		}
		if res != nil && res.IsSelect {
			r.lastCols, r.lastRows = res.Columns, res.Rows
			r.lastTruncated = res.Truncated
			r.recordResultProvenance(sql)
		}
		if r.expanded {
			render.Vertical(r.out, res, r.maxRows, r.timing)
		} else {
			render.Single(r.out, res, r.maxRows, r.timing)
		}
		return
	}
	results := r.fanoutLimit(r.targets, sql, true, timeout, readLimit)
	// Сохраняем экспортируемый результат, только если хотя бы один шард реально
	// вернул SELECT; иначе (все шарды с ошибкой / не-select) оставляем очищенным,
	// чтобы \export отказал, а не записал результат из одних заголовков или устаревший.
	if anySelect(results) {
		r.lastCols, r.lastRows = render.Merge(results)
		r.lastTruncated = anyTruncated(results)
		r.recordResultProvenance(sql)
	}
	render.Multi(r.out, results, r.maxRows)
}

// clearLastResult сбрасывает экспортируемый результат (колонки/строки/усечение/цели).
// lastQuery здесь не трогается — это происхождение для \save и очищается только
// при смене контекста; recordResultProvenance заново ставит его на свежем результате.
func (r *REPL) clearLastResult() {
	r.lastCols, r.lastRows, r.lastTruncated, r.lastTargets = nil, nil, false, nil
}

// recordResultProvenance помечает только что полученный экспортируемый результат
// запросом и точными физическими целями, откуда он пришёл, чтобы поздний
// усечённый \export перечитал ТОТ ЖЕ запрос на ТЕХ ЖЕ шардах, даже если шёл
// \watch (который попадает в execReadCtx напрямую) или контекст потом сменился.
func (r *REPL) recordResultProvenance(sql string) {
	r.lastQuery = strings.TrimSpace(sql)
	r.lastTargets = append([]cluster.Shard(nil), r.targets...)
}

// anyTruncated сообщает, был ли результат какого-либо шарда урезан (есть ещё строки).
func anyTruncated(results []db.ShardResult) bool {
	for _, sr := range results {
		if sr.Err == nil && sr.Result != nil && sr.Result.Truncated {
			return true
		}
	}
	return false
}

// anySelect сообщает, вернул ли какой-либо шард успешный результат SELECT.
func anySelect(results []db.ShardResult) bool {
	for _, sr := range results {
		if sr.Err == nil && sr.Result != nil && sr.Result.IsSelect {
			return true
		}
	}
	return false
}

// errWriteRefused — execWrite ОТКЛОНИЛ выполнение ДО запуска на шардах
// (запрещённая операция, tx-control/session-state, mixed-файл, нетранзакционная
// миграция на prod без дедлайна, отмена пользователем, сбой сборки обёртки). Это
// принципиально НЕ то же самое, что «выполнили и получили пустой список»: вызывающий
// ОБЯЗАН трактовать это как незавершённую операцию (pending/ошибка), а не как успех.
// Подробное объяснение причины уже напечатано в r.out внутри execWrite; эта ошибка
// нужна только для управления потоком у вызывающих (см. runMigrationFile,
// runStagedRollout).
var errWriteRefused = errors.New("write refused before execution")

// execWrite выполняет запись/миграцию на каждой текущей цели. При wrap=true SQL —
// это тело, и оно получает защитную конструкцию "set role + statement_timeout"
// (отправляется одним exec); при false — отправляется дословно (без обёртки,
// например \i). Операторы, которым нужно выполняться вне транзакции (CONCURRENTLY,
// VACUUM, ...), определяются автоматически и выполняются отдельными autocommit
// exec без обёртки, роли и таймаута.
//
// Второе значение — ошибка ОТКАЗА: ненулевое, если выполнение отклонено до запуска
// на шардах (errWriteRefused или ошибка сборки обёртки). При nil выполнение реально
// состоялось, а сами шардовые ошибки лежат в ExecResult.Err каждой записи. Так
// вызывающий не спутает «отказано» с «успешно вернулось 0 результатов».
func (r *REPL) execWrite(sql string, wrap bool) ([]db.ExecResult, error) {
	// Очищаем любой прошлый экспортируемый результат перед записью, чтобы SELECT,
	// выполненный ранее, не оставался экспортируемым после завершения миграции.
	r.clearLastResult()
	// DROP DATABASE необратимо уничтожает всю базу шарда; terox запрещает его
	// БЕЗУСЛОВНО — на любом пути (обёрнутая запись, staged-rollout, \i дословно),
	// даже в write-режиме с подтверждением. Проверяем ДО разбора и выполнения.
	if op := migration.ForbiddenOperation(sql); op != "" {
		r.refuseForbidden(op)
		return nil, errWriteRefused
	}
	// Session-state firewall + tx-control backstop ДО любого разбора/выполнения:
	// self-tx и session-scoped конструкции нельзя оборачивать и они не должны
	// проскользнуть даже по НЕтранзакционной ветке (например DISCARD ALL, которая
	// одновременно IsNonTransactional). Вызывающие отклоняют это до подтверждения;
	// здесь — финальная страховка, чтобы обёртку НИКОГДА не обошли молча.
	if wrap {
		if migration.HasTxControl(sql) {
			r.refuseTxControl()
			return nil, errWriteRefused
		}
		if reason := migration.SessionStateViolation(sql); reason != "" {
			r.refuseSessionState(reason)
			return nil, errWriteRefused
		}
	}
	plan := migration.Classify(sql)
	if plan.Mixed {
		fmt.Fprintln(r.out, "mixed file: it blends CONCURRENTLY/VACUUM with transactional statements.")
		fmt.Fprintln(r.out, "split the non-transactional statements into a separate file — they cannot share a transaction.")
		return nil, errWriteRefused
	}
	if plan.NonTransactional {
		// Эти операторы (CREATE INDEX CONCURRENTLY, VACUUM, REINDEX CONCURRENTLY,
		// ...) не могут выполняться внутри транзакционного блока, так что защитная
		// конструкция с begin здесь невозможна — а при транзакционном пуллинге
		// pgbouncer отдельный SET до них всё равно не дойдёт. Они выполняются БЕЗ
		// ЗАЩИТЫ (без роли / statement_timeout / lock_timeout); их ограничивает
		// только клиентский migration_timeout. Сообщаем об этом громко и требуем
		// явное дополнительное подтверждение на проде.
		fmt.Fprintln(r.out, ui.Danger.Render("⚠ non-transactional statement(s) — cannot run in a transaction,"))
		fmt.Fprintln(r.out, "  so they run WITHOUT the set role + statement_timeout + lock_timeout wrapper (only the client migration_timeout applies).")
		if r.prod {
			// Серверный таймаут здесь невозможен, поэтому ЕДИНСТВЕННОЕ ограничение —
			// клиентский migration_timeout. При 0 застрявший CONCURRENTLY/VACUUM
			// висит на проде вечно, поэтому требуем ненулевой дедлайн перед запуском.
			if r.cfg.MigrationTimeout <= 0 {
				fmt.Fprintln(r.out, ui.Danger.Render("refused: non-transactional migration on PROD has no time bound"))
				fmt.Fprintln(r.out, "  these statements cannot use the server statement_timeout, and migration_timeout is 0 (unbounded).")
				fmt.Fprintln(r.out, "  set a client deadline in config.yaml (e.g. migration_timeout: 30m) before running this on prod.")
				return nil, errWriteRefused
			}
			fmt.Fprintf(r.out, "  this runs UNPROTECTED on PROD across %d shard(s) [%s] (client migration_timeout %s).\n", len(r.targets), r.targetLabel, time.Duration(r.cfg.MigrationTimeout))
			if strings.TrimSpace(r.readLine("Type 'unprotected' to proceed: ")) != "unprotected" {
				fmt.Fprintln(r.out, "cancelled")
				return nil, errWriteRefused
			}
		}
		results := r.runForEach(func(ctx context.Context, s cluster.Shard) (int64, error) {
			return r.mgr.ExecScript(ctx, s, plan.Statements)
		})
		// Сбойный CREATE INDEX CONCURRENTLY оставляет за собой INVALID-индекс,
		// который занимает место и игнорируется планировщиком. Если среди операторов
		// был такой CREATE и хотя бы один шард упал — напоминаем про \heal, который
		// найдёт и уберёт остатки.
		if hasCreateIndexConcurrently(plan.Statements) && anyExecError(results) {
			r.warnInvalidIndexLeftover()
		}
		return results, nil
	}
	payload := sql
	if wrap {
		built, err := migration.BuildTransactional(sql, r.role(), r.stmtTimeout, r.cfg.LockTimeout)
		if err != nil {
			fmt.Fprintf(r.out, "migration aborted: %v\n", err)
			return nil, err
		}
		payload = built
	}
	return r.runForEach(func(ctx context.Context, s cluster.Shard) (int64, error) {
		return r.mgr.ExecOnce(ctx, s, payload)
	}), nil
}

// runForEach выполняет fn на каждой цели (одна -> одна строка; много -> таблица
// статуса по шардам) с продолжением при ошибке, возвращая результаты по шардам.
func (r *REPL) runForEach(fn func(context.Context, cluster.Shard) (int64, error)) []db.ExecResult {
	// Записи прерываются по Ctrl-C, как и чтения: медленную/зависшую миграцию можно
	// прервать, отменив выполняющиеся и ещё не начатые шарды.
	ctx, cancel := interruptible()
	defer cancel()
	timeout := time.Duration(r.cfg.MigrationTimeout) // 0 => полагаться на серверный statement_timeout
	if len(r.targets) == 1 {
		cctx, c2 := contextWithOptionalTimeout(ctx, timeout)
		defer c2()
		start := time.Now()
		affected, err := fn(cctx, r.targets[0])
		res := db.ExecResult{Shard: r.targets[0], Affected: affected, Err: err, Duration: time.Since(start)}
		render.WriteSingle(r.out, res)
		return []db.ExecResult{res}
	}
	results := r.mgr.ForEachShard(ctx, r.targets, r.cfg.Concurrency(len(r.targets)), timeout, r.cfg.StopWritesOnError(), fn)
	render.ExecResults(r.out, results)
	return results
}

// role возвращает роль записи, до которой повышаться для текущего хранилища, или
// "" если её нет. Она СТРОГО на хранилище (Storage.MigrationRole) и не привязана
// к флагу prod: если хранилище не задаёт роль, `set role` не выполняется вообще.
// (Роль полной записи с минимальными привилегиями — выбор развёртывания, поэтому
// terox её не предполагает.)
func (r *REPL) role() string {
	return r.migrationRole
}

// confirmWrite запрашивает простое подтверждение перед записью: y/yes.
func (r *REPL) confirmWrite() bool {
	target := r.targets[0].Label
	if len(r.targets) > 1 {
		target = fmt.Sprintf("%d шард(а/ов) [%s]", len(r.targets), r.targetLabel)
	}
	ans := r.readLine(fmt.Sprintf("Записать на %s? [y/N] ", target))
	switch strings.ToLower(strings.TrimSpace(ans)) {
	case "y", "yes":
		return true
	}
	return false
}

// confirmUnqualified — более строгий барьер для UPDATE/DELETE без WHERE (или
// TRUNCATE): он затрагивает все строки, поэтому требует ввести "yes".
func (r *REPL) confirmUnqualified() bool {
	warn := "⚠ нет WHERE — это затронет ВСЕ строки"
	if ui.Enabled {
		warn = ui.Danger.Render(warn)
	}
	fmt.Fprintf(r.out, "%s на %d шард(а/ов) [%s]\n", warn, len(r.targets), r.targetLabel)
	typed := r.readLine("Введите 'yes' для записи во ВСЕ строки: ")
	return strings.TrimSpace(typed) == "yes"
}

// readLine читает одну строку с временным приглашением.
func (r *REPL) readLine(prompt string) string {
	r.rl.SetPrompt(prompt)
	line, _ := r.rl.Readline()
	return line
}

func historyPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	d := filepath.Join(dir, "terox")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	hist := filepath.Join(d, "history")
	// Файл истории может содержать текст запросов с буквальными секретами.
	// Заранее создаём его с правами 0600 (и ужесточаем существующий), чтобы он
	// оставался приватным — дозапись сохраняет режим.
	if f, err := os.OpenFile(hist, os.O_CREATE, 0o600); err == nil {
		_ = f.Close()
	}
	_ = os.Chmod(hist, 0o600)
	return hist, nil
}

// recordHistory — ЕДИНСТВЕННАЯ точка, через которую что-либо попадает в историю.
// Сохраняет завершённый оператор в дисковую (readline) и, для tea-редактора, в
// in-memory историю, но НИКОГДА — оператор, похожий на содержащий секрет
// (CREATE/ALTER ROLE/USER, PASSWORD, ...). Отдельные строки ввода и незавершённые
// многострочные операторы сюда не приходят, поэтому ни диск, ни память не хранят
// пароль или обрывок запроса; tea и readline получают одинаковую семантику
// «история целыми операторами».
//
// В память пишем только в режиме tea: в режиме readline r.history остаётся nil,
// чтобы переключение \editor tea лениво подгрузило полную дисковую историю (куда
// readline уже сохранил операторы этой сессии), а не только текущую сессию.
func (r *REPL) recordHistory(sql string) {
	sql = strings.TrimSpace(sql)
	if sql == "" || r.historyOff || isSensitiveStatement(sql) {
		return
	}
	if r.rl != nil {
		r.rl.SaveHistory(sql)
	}
	if r.useTeaEditor {
		r.history = append(r.history, sql)
	}
}

// runHistory обрабатывает \history [clear|off|on]: показывает состояние и
// подсказку, очищает историю (диск + память + кольцо readline) или включает/
// выключает запись на текущую сессию.
func (r *REPL) runHistory(args []string) {
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}
	switch sub {
	case "":
		state := "on"
		if r.historyOff {
			state = "off"
		}
		fmt.Fprintf(r.out, "history recording: %s. Up/Down to navigate, Ctrl-R to search.\n", state)
		fmt.Fprintln(r.out, "  \\history clear   wipe stored history (disk + this session)")
		fmt.Fprintln(r.out, "  \\history off|on  stop/resume recording for this session")
	case "clear":
		r.clearHistory()
		fmt.Fprintln(r.out, "history cleared.")
	case "off":
		r.historyOff = true
		fmt.Fprintln(r.out, "history recording off for this session (\\history on to resume).")
	case "on":
		r.historyOff = false
		fmt.Fprintln(r.out, "history recording on.")
	default:
		fmt.Fprintf(r.out, "unknown \\history option %q (use: clear, off, on)\n", sub)
	}
}

// clearHistory стирает историю в памяти (tea), кольцо readline и файл на диске,
// чтобы случайно набранный секрет можно было убрать без перезапуска.
func (r *REPL) clearHistory() {
	r.history = nil
	if r.rl != nil {
		r.rl.ResetHistory()
	}
	if r.histPath != "" {
		if f, err := os.OpenFile(r.histPath, os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
			_ = f.Close()
		}
	}
}

// isSensitiveStatement сообщает, содержит ли sql скорее всего учётные данные/
// секрет, чтобы держать его ВНЕ сохраняемого файла истории. Ложные срабатывания
// стоят лишь пропущенной записи в истории, поэтому совпадение намеренно широкое.
func isSensitiveStatement(sql string) bool {
	up := strings.ToUpper(sql)
	for _, kw := range []string{"PASSWORD", "SECRET", "WITH PASSWORD", "SET PASSWORD",
		"CREATE USER", "ALTER USER", "CREATE ROLE", "ALTER ROLE", "ENCRYPTED PASSWORD"} {
		if strings.Contains(up, kw) {
			return true
		}
	}
	return false
}

func oneLine(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
}

// huhSelect показывает приглашение с одиночным выбором. aborted=true, когда
// пользователь нажал Esc / Ctrl-C (используется, чтобы подняться на уровень
// выше в любом меню). Фильтрация выключена, чтобы Esc не перехватывался
// фильтром и всегда прерывал.
func huhSelect(title string, options []string) (choice string, aborted bool, err error) {
	opts := make([]huh.Option[string], len(options))
	for i, o := range options {
		opts[i] = huh.NewOption(o, o)
	}
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"))
	err = huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title(title).Height(12).
			Filtering(false).Options(opts...).Value(&choice),
	)).WithKeyMap(km).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return "", true, nil
	}
	return choice, false, err
}
