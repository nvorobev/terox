// Command terox — интерактивный многошардовый клиент PostgreSQL: выбор сервиса,
// хранилища и шарда (или всех шардов) и запуск запросов с историей,
// контролем записи и объединённым выводом результатов.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mattn/go-isatty"

	"terox/internal/config"
	"terox/internal/preflight"
	"terox/internal/repl"
	"terox/internal/wizard"
)

// version — версия сборки, которую печатает `terox version`. Единственный
// источник истины: эту строку и сообщает бинарник.
var version = "1.0.0"

// stringSlice — повторяемый строковый флаг (напр. --allow-warning CODE --allow-warning CODE).
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ", ") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		// -h/--help печатает справку и возвращает ErrHelp — это успешный
		// вызов, а не ошибка. Выходим с кодом 0 без лишнего сообщения.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "terox:", err)
		// Частичный успех многошардового запроса (часть шардов ответила) — отдельный
		// exit code 2, чтобы CI отличал неполный результат от полного провала/ошибки
		// конфигурации (exit 1). Feature 13.
		var pe *repl.PartialError
		if errors.As(err, &pe) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	// Сообщаем версию сборки пакету repl (для машиночитаемого отчёта раскатки).
	repl.SetVersion(version)
	// Печатаем версию сборки до всего остального (конфиг не нужен).
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version", "-v":
			printVersion()
			return nil
		}
	}

	fs := flag.NewFlagSet("terox", flag.ContinueOnError)
	fs.Usage = printUsage
	configPath := fs.String("c", "", "config file path (default: $TEROX_CONFIG or ~/.config/terox/config.yaml)")
	target := fs.String("t", "", "startup context service/storage[/selector], skips the pickers")
	format := fs.String("format", "table", "non-interactive output format for `terox query`: table|json|csv|envelope")
	strict := fs.Bool("strict", false, "treat config warnings as errors in the preflight")
	var allowWarn stringSlice
	fs.Var(&allowWarn, "allow-warning", "acknowledge a config warning by CODE so it does not fail --strict (repeatable; see 'terox validate')")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()

	// version и help не требуют конфига и должны работать в т.ч. ПОСЛЕ глобальных
	// флагов (`terox -c missing.yaml version`). Обрабатываем их здесь — до
	// resolve/load конфига, — чтобы отсутствующий или ошибочный конфиг их не ломал.
	if len(rest) > 0 {
		switch rest[0] {
		case "version":
			printVersion()
			return nil
		case "help":
			printUsage()
			return nil
		}
	}

	path, err := config.ResolvePath(*configPath)
	if err != nil {
		return err
	}
	cfg, loadErr := config.Load(path)
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return loadErr
	}

	// Подкоманды.
	if len(rest) > 0 {
		switch rest[0] {
		case "add":
			// --strict и --allow-warning принимаются и после `add` (как у других
			// подкоманд), иначе они бы молча отбрасывались.
			afs := flag.NewFlagSet("add", flag.ContinueOnError)
			astrict := afs.Bool("strict", *strict, "treat config warnings as errors")
			aallow := allowWarn
			afs.Var(&aallow, "allow-warning", "acknowledge a config warning CODE so it does not fail --strict (repeatable)")
			if err := afs.Parse(rest[1:]); err != nil {
				return err
			}
			svc, sto, err := wizard.Run(cfg)
			if err != nil {
				return err
			}
			fmt.Printf("Added %s/%s to %s\n", svc, sto, cfg.Path())
			// Сразу показываем проблемы валидации добавленного кластера (особенно
			// TLS), не откладывая их до следующего запуска. Конфиг уже сохранён
			// мастером, поэтому при ошибках возвращаем non-zero — иначе автоматизация
			// не заметила бы записанный невалидный конфиг. Подтверждённые через
			// --allow-warning предупреждения выводятся отдельно и не валят --strict.
			allow := preflight.ParseAllowWarning(aallow)
			fs := preflight.Run(cfg)
			errs, warns, overrides := preflight.Partition(fs, allow)
			for _, w := range warns {
				fmt.Fprintf(os.Stderr, "config warning: %s\n", w.Message)
			}
			for _, o := range overrides {
				fmt.Fprintf(os.Stderr, "config: [%s] %s (acknowledged via --allow-warning)\n", o.Code, o.Message)
			}
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "config error: %s\n", e.Message)
			}
			if len(errs) > 0 {
				return fmt.Errorf("saved %s/%s but the config now has %d error(s) — run 'terox validate'", svc, sto, len(errs))
			}
			if *astrict && len(warns) > 0 {
				return fmt.Errorf("saved %s/%s but the config has %d warning(s) under --strict", svc, sto, len(warns))
			}
			return nil
		case "validate":
			vfs := flag.NewFlagSet("validate", flag.ContinueOnError)
			asJSON := vfs.Bool("json", false, "emit machine-readable JSON (for CI)")
			vstrict := vfs.Bool("strict", *strict, "treat warnings as errors")
			vallow := allowWarn
			vfs.Var(&vallow, "allow-warning", "acknowledge a config warning CODE so it does not fail --strict (repeatable)")
			if err := vfs.Parse(rest[1:]); err != nil {
				return err
			}
			return validateConfig(cfg, *asJSON, *vstrict, preflight.ParseAllowWarning(vallow))
		case "query":
			// Разбираем флаги после подкоманды (terox query -t T --format json <sql>),
			// беря глобальные -t/--format как значения по умолчанию.
			qfs := flag.NewFlagSet("query", flag.ContinueOnError)
			qt := qfs.String("t", *target, "target service/storage[/selector]")
			qf := qfs.String("format", *format, "output format: table|json|csv|envelope")
			qorder := qfs.String("order-by", "", "global sort column [:asc|:desc] across shards (per-shard ORDER BY is not global)")
			qmode := qfs.String("mode", "", "shard result mode: union(=union-by-name, default) | strict | merge-sort | quorum | aggregate | first-success | per-shard")
			// --strict принимается и до, и после подкоманды (как у validate),
			// чтобы `terox query --strict ...` не падал с непонятной ошибкой.
			qstrict := qfs.Bool("strict", *strict, "treat config warnings as errors")
			qallow := allowWarn
			qfs.Var(&qallow, "allow-warning", "acknowledge a config warning CODE so it does not fail --strict (repeatable)")
			if err := qfs.Parse(rest[1:]); err != nil {
				return err
			}
			if bad := misplacedFlag(qfs.Args(), "t", "format", "order-by", "mode", "strict", "allow-warning"); bad != "" {
				return fmt.Errorf("terox query: flag %s must come before the SQL (e.g. `terox query -t T --format json <sql>`), otherwise it is read as part of the query", bad)
			}
			if *qt == "" {
				return fmt.Errorf("terox query needs -t <service/storage[/selector]>")
			}
			sql := strings.Join(qfs.Args(), " ")
			if strings.TrimSpace(sql) == "" {
				// Нет позиционного SQL: если stdin не терминал (пайп/heredoc) — читаем
				// его как запрос. Удобно для CI/Makefile без экранирования кавычек/$.
				if !isatty.IsTerminal(os.Stdin.Fd()) {
					data, rerr := io.ReadAll(os.Stdin)
					if rerr != nil {
						return fmt.Errorf("terox query: reading SQL from stdin: %w", rerr)
					}
					sql = string(data)
				}
				if strings.TrimSpace(sql) == "" {
					return fmt.Errorf("usage: terox query -t <target> [--format table|json|csv|envelope] <sql>   (or pipe SQL on stdin)")
				}
			}
			// Единый preflight ДО подключения: query/plan обязаны давать те же
			// гарантии TLS/таймаутов/лимитов, что и интерактивный REPL.
			if err := gateConfigOpts(cfg, preflight.Options{Strict: *qstrict, AllowWarning: preflight.ParseAllowWarning(qallow)}); err != nil {
				return err
			}
			return repl.Query(cfg, *qt, sql,
				repl.QueryOptions{Format: *qf, OrderBy: *qorder, Mode: *qmode}, os.Stdout)
		case "plan":
			pfs := flag.NewFlagSet("plan", flag.ContinueOnError)
			pt := pfs.String("t", *target, "target service/storage[/selector]")
			analyze := pfs.Bool("analyze", false, "run EXPLAIN ANALYZE (executes the query)")
			pstrict := pfs.Bool("strict", *strict, "treat config warnings as errors")
			pallow := allowWarn
			pfs.Var(&pallow, "allow-warning", "acknowledge a config warning CODE so it does not fail --strict (repeatable)")
			if err := pfs.Parse(rest[1:]); err != nil {
				return err
			}
			if bad := misplacedFlag(pfs.Args(), "t", "analyze", "strict", "allow-warning"); bad != "" {
				return fmt.Errorf("terox plan: flag %s must come before the query (e.g. `terox plan -t T --analyze <query>`), otherwise it is read as part of the query", bad)
			}
			if *pt == "" {
				return fmt.Errorf("terox plan needs -t <service/storage[/selector]>")
			}
			if len(pfs.Args()) == 0 {
				return fmt.Errorf("usage: terox plan -t <target> [--analyze] <query>")
			}
			if err := gateConfigOpts(cfg, preflight.Options{Strict: *pstrict, AllowWarning: preflight.ParseAllowWarning(pallow)}); err != nil {
				return err
			}
			return repl.Plan(cfg, *pt, strings.Join(pfs.Args(), " "), *analyze, os.Stdout)
		case "migrate":
			// Headless migrate — ТОЛЬКО offline-предпросмотр (payload + план раскатки),
			// без подключения к БД и без применения: безопасная валидация миграций в CI.
			// Реальное применение остаётся за интерактивным `\migrate --allowed`.
			mfs := flag.NewFlagSet("migrate", flag.ContinueOnError)
			mt := mfs.String("t", *target, "target service/storage[/selector]")
			mcanary := mfs.Bool("canary", false, "preview a canary first stage (one shard), then the rest")
			mbatch := mfs.Int("batch", 0, "preview batches of N shards per stage (0 = one stage)")
			mresume := mfs.Bool("resume", false, "preview only shards not yet applied (per local ledger)")
			mstrict := mfs.Bool("strict", *strict, "treat config warnings as errors")
			mallow := allowWarn
			mfs.Var(&mallow, "allow-warning", "acknowledge a config warning CODE so it does not fail --strict (repeatable)")
			if err := mfs.Parse(rest[1:]); err != nil {
				return err
			}
			if bad := misplacedFlag(mfs.Args(), "t", "canary", "batch", "resume", "strict", "allow-warning"); bad != "" {
				return fmt.Errorf("terox migrate: flag %s must come before the file (e.g. `terox migrate -t T --canary <file.sql>`), otherwise it is read as the file path", bad)
			}
			if *mt == "" {
				return fmt.Errorf("terox migrate needs -t <service/storage[/selector]>")
			}
			if len(mfs.Args()) != 1 {
				return fmt.Errorf("usage: terox migrate -t <target> [--canary|--batch N] [--resume] <file.sql>")
			}
			if err := gateConfigOpts(cfg, preflight.Options{Strict: *mstrict, AllowWarning: preflight.ParseAllowWarning(mallow)}); err != nil {
				return err
			}
			return repl.MigratePreview(cfg, *mt, mfs.Args()[0],
				repl.MigrateOptions{Canary: *mcanary, Batch: *mbatch, Resume: *mresume}, os.Stdout)
		default:
			return fmt.Errorf("unknown command %q (try: terox, terox add, terox validate, terox query, terox plan, terox migrate)", rest[0])
		}
	}

	// Интерактивный режим проходит тот же preflight, что query/plan, и
	// отказывается стартовать с ошибочным конфигом, чтобы дубль цели или опечатка
	// не дошли до записи в прод. Пустой конфиг — исключение: REPL запускает мастер
	// настройки.
	if err := gateConfigOpts(cfg, preflight.Options{Strict: *strict, AllowEmpty: true, AllowWarning: preflight.ParseAllowWarning(allowWarn)}); err != nil {
		return err
	}

	r, err := repl.New(cfg)
	if err != nil {
		return err
	}
	if *target != "" {
		r.SetStartupTarget(*target)
	}
	return r.Run()
}

// gateConfig — короткая обёртка над gateConfigOpts без allow-list (3-аргументный
// вызов, который используют тесты и удобно вызывать из простых путей).
func gateConfig(cfg *config.Config, allowEmpty, strict bool) error {
	return gateConfigOpts(cfg, preflight.Options{Strict: strict, AllowEmpty: allowEmpty})
}

// gateConfigOpts печатает отчёт preflight в stderr и отказывается продолжать при
// наличии ошибок (а под strict — при любых НЕподавленных предупреждениях). Это
// единственный шлюз перед любым путём, создающим соединение с БД: REPL, query и plan
// вызывают его ДО подключения, поэтому невалидный/небезопасный конфиг одинаково
// блокирует их все. Подавленные через --allow-warning предупреждения печатаются
// отдельной категорией override и не считаются под --strict.
func gateConfigOpts(cfg *config.Config, opt preflight.Options) error {
	fs := preflight.Run(cfg)
	errs, warns, overrides := preflight.Partition(fs, opt.AllowWarning)
	for _, f := range errs {
		fmt.Fprintf(os.Stderr, "config error: %s\n", f.Message)
	}
	if opt.Strict {
		for _, f := range warns {
			fmt.Fprintf(os.Stderr, "config warning: %s\n", f.Message)
		}
	} else if len(warns) > 0 {
		fmt.Fprintf(os.Stderr, "config: %d warning(s) — run 'terox validate' for details\n", len(warns))
	}
	for _, f := range overrides {
		fmt.Fprintf(os.Stderr, "config: [%s] %s (acknowledged via --allow-warning)\n", f.Code, f.Message)
	}
	if unknown := preflight.UnknownAllowCodes(opt.AllowWarning); len(unknown) > 0 {
		fmt.Fprintf(os.Stderr, "config: unknown --allow-warning code(s): %s\n", strings.Join(unknown, ", "))
	}
	return preflight.Gate(cfg, fs, opt)
}

// validateConfig печатает полный отчёт валидации (для человека или в JSON для CI) и
// завершается с ошибкой при наличии ошибок (а под strict — и при любых неподавленных
// предупреждениях). JSON-схема стабильна и расширена: к полям config/ok/errors/
// warnings (строки, для совместимости) добавлены структурированные findings и
// overrides с полями code/severity/path/message.
func validateConfig(cfg *config.Config, asJSON, strict bool, allow map[string]bool) error {
	fs := preflight.Run(cfg)
	errsF, warnsF, overridesF := preflight.Partition(fs, allow)
	failed := len(errsF) > 0 || (strict && len(warnsF) > 0)
	if asJSON {
		type findingJSON struct {
			Code     string `json:"code"`
			Severity string `json:"severity"`
			Path     string `json:"path,omitempty"`
			Message  string `json:"message"`
		}
		toJSON := func(in []config.Finding) []findingJSON {
			out := make([]findingJSON, 0, len(in))
			for _, f := range in {
				out = append(out, findingJSON{f.Code, f.Severity.String(), f.Path, f.Message})
			}
			return out
		}
		report := struct {
			Config    string        `json:"config"`
			OK        bool          `json:"ok"`
			Errors    []string      `json:"errors"`
			Warnings  []string      `json:"warnings"`
			Findings  []findingJSON `json:"findings"`
			Overrides []findingJSON `json:"overrides"`
		}{
			Config:    cfg.Path(),
			OK:        !failed,
			Errors:    messagesOf(errsF),
			Warnings:  messagesOf(warnsF),
			Findings:  toJSON(append(append([]config.Finding{}, errsF...), warnsF...)),
			Overrides: toJSON(overridesF),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
		if len(errsF) > 0 {
			return fmt.Errorf("config has %d error(s)", len(errsF))
		}
		if strict && len(warnsF) > 0 {
			return fmt.Errorf("config has %d warning(s) under --strict", len(warnsF))
		}
		return nil
	}
	fmt.Printf("config: %s\n", cfg.Path())
	for _, f := range warnsF {
		fmt.Printf("  warning [%s]: %s\n", f.Code, f.Message)
	}
	for _, f := range overridesF {
		fmt.Printf("  override [%s]: %s (acknowledged)\n", f.Code, f.Message)
	}
	for _, f := range errsF {
		fmt.Printf("  ERROR  [%s]: %s\n", f.Code, f.Message)
	}
	fmt.Printf("%d error(s), %d warning(s), %d override(s)\n", len(errsF), len(warnsF), len(overridesF))
	if unknown := preflight.UnknownAllowCodes(allow); len(unknown) > 0 {
		fmt.Printf("note: unknown --allow-warning code(s): %s\n", strings.Join(unknown, ", "))
	}
	if len(errsF) > 0 {
		return fmt.Errorf("config has %d error(s)", len(errsF))
	}
	if strict && len(warnsF) > 0 {
		return fmt.Errorf("config has %d warning(s) under --strict", len(warnsF))
	}
	return nil
}

// misplacedFlag ищет среди позиционных аргументов токен, выглядящий как один из
// известных флагов подкоманды (имена без ведущих дефисов, форма name или name=val).
// Пакет flag прекращает разбор на первом позиционном аргументе, поэтому флаг,
// поставленный ПОСЛЕ SQL/файла, молча уходит в полезную нагрузку. Эта проверка
// превращает тихую ошибку в понятное сообщение. SQL-токены, начинающиеся с дефиса
// (-1, оператор -, комментарий --text), под известные имена флагов не подпадают.
func misplacedFlag(args []string, names ...string) string {
	known := make(map[string]bool, len(names))
	for _, n := range names {
		known[n] = true
	}
	for _, a := range args {
		if len(a) < 2 || a[0] != '-' {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if known[name] {
			return a
		}
	}
	return ""
}

// messagesOf извлекает строки сообщений находок (nil→[] для стабильной JSON-схемы).
func messagesOf(fs []config.Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Message)
	}
	return out
}

// printVersion печатает версию сборки — единая точка, чтобы строка не разъехалась
// между ранней (до флагов) и поздней (после флагов) диспетчеризацией.
func printVersion() { fmt.Println("terox", version) }

func printUsage() {
	fmt.Print(`terox — interactive multi-shard PostgreSQL client

Usage:
  terox                       start the interactive REPL (pick context via menus)
  terox -t item/sharded/all   start directly in a context, skipping the menus
  terox -t item/sharded/0,1,5 start targeting a shard subset
  terox -c /path/config.yaml  use a specific config file
  terox add                   register a new cluster via the wizard
  terox validate [--strict]   validate the config file (--strict fails on warnings)
  terox query -t T --format json <sql>   run a read-only query non-interactively
  terox plan -t T [--analyze] <query>    print an analyzed EXPLAIN plan (machine JSON)
  terox migrate -t T [--canary|--batch N] [--resume] <file.sql>
                                         offline-preview a migration's exact payload and rollout
                                         plan (no DB, no apply; validate migrations in CI)

Flags:
  -c PATH        config file
  -t SPEC        startup context service/storage[/selector]
  --format FMT   output format for 'query': table|json|csv|envelope
  --order-by C   global sort column [:asc|:desc] for 'query' (per-shard ORDER BY is not global)
  --mode M       shard result mode for 'query': union|union-by-name|strict|merge-sort|
                 quorum|aggregate|first-success|per-shard (see docs/shard-semantics.md).
                 For --mode quorum give a stable ORDER BY: without it PostgreSQL does not
                 guarantee row order and identical data across shards may look divergent.
  --strict       treat config warnings as errors in the preflight (before or after the subcommand)
  --allow-warning CODE  acknowledge a config warning by its stable CODE so --strict
                 does not fail on it (repeatable; codes are shown by 'terox validate')

Flags for query/plan/migrate must come BEFORE the SQL/file (e.g. terox query -t T
--format json <sql>); a flag placed after the SQL would be read as part of it.

Every command that connects to a database (the REPL, query, plan) runs the same
config preflight first; help and version need no config. Config search order:
-c, $TEROX_CONFIG, ./config.yaml, <exe-dir>/config.yaml, then ~/.config/terox/config.yaml.
`)
}
