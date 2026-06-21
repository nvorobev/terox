// Package config — модель конфигурации terox и её хранение в YAML.
package config

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config — корневая конфигурация terox, хранится в YAML.
type Config struct {
	// WriteModeDefault включает режим записи при старте.
	WriteModeDefault bool `yaml:"write_mode_default"`
	// WriteApprove управляет подтверждением перед записью. Указатель: явный
	// `write_approve: false` отключает запрос подтверждения, nil (отсутствие) —
	// включено. Переключается командой \write_approve. Получать через WriteApproveEnabled().
	WriteApprove *bool `yaml:"write_approve,omitempty"`
	// Editor выбирает интерактивный редактор строки: "tea" — редактор с живым
	// выпадающим автодополнением, любое другое значение (по умолчанию) — классический
	// readline. Переопределяется через TEROX_EDITOR и переключается командой \editor.
	Editor string `yaml:"editor,omitempty"`
	// AutoKeymap конвертирует кириллический ввод в латинскую раскладку (ЙЦУКЕН→
	// QWERTY) вне строковых литералов, чтобы команда/SQL, набранные на русской
	// раскладке, всё равно работали. Указатель: явный `auto_keymap: false` отключает,
	// nil (отсутствие) — включено. Получать через AutoKeymapEnabled().
	AutoKeymap *bool `yaml:"auto_keymap,omitempty"`
	// Timing управляет выводом длительности запроса при старте (как команда \timing).
	// Указатель: nil (отсутствие) — включено. Получать через TimingEnabled().
	Timing *bool `yaml:"timing,omitempty"`
	// Impact управляет предпросмотром числа затрагиваемых строк перед UPDATE/DELETE
	// при старте (как команда \impact). Указатель: nil (отсутствие) — выключено.
	// Получать через ImpactEnabled().
	Impact *bool `yaml:"impact,omitempty"`
	// Suggest управляет inline ghost-автоподсказкой при старте (как команда \suggest).
	// Указатель: nil (отсутствие) — включено. Получать через SuggestEnabled().
	Suggest *bool `yaml:"suggest,omitempty"`
	// Expanded задаёт стартовый развёрнутый (по полю на строку) вывод (как команда \x).
	// Указатель: nil (отсутствие) — выключено. Получать через ExpandedDefault().
	Expanded *bool `yaml:"expanded,omitempty"`
	// MaxRows ограничивает число выводимых строк одного запроса. Указатель: явный
	// `max_rows: 0` (без ограничения) отличается от отсутствия ключа (1000).
	// Читать через MaxRowsValue().
	MaxRows *int `yaml:"max_rows"`
	// FanoutMode задаёт выполнение запросов по нескольким шардам: "parallel"
	// (по умолчанию — все шарды сразу, каждый имеет свой пул pgbouncer/backend) или
	// "sequential" (по одному шарду по порядку).
	FanoutMode string `yaml:"fanout_mode,omitempty"`
	// FanoutConcurrency — необязательное ограничение числа одновременных шардов в
	// параллельном режиме. 0 (по умолчанию) — без ограничения. В sequential игнорируется.
	FanoutConcurrency int `yaml:"fanout_concurrency,omitempty"`
	// QueryTimeout — таймаут запроса на каждый шард.
	QueryTimeout Duration `yaml:"query_timeout"`

	// Глобальные настройки миграций (переопределяются per-storage). Используются
	// \migrate и записями для построения защитного префикса:
	//   set local role <role>;   -- только если STORAGE задаёт роль (Storage.MigrationRole)
	//   set local statement_timeout='<StatementTimeout>'; [set local lock_timeout='<LockTimeout>';]
	//
	// Роль записи задаётся строго per-storage (см. Storage.MigrationRole); общей роли
	// на кластер и значения по умолчанию нет — storage без роли не выполняет `set role`.
	StatementTimeout string `yaml:"statement_timeout,omitempty"`
	LockTimeout      string `yaml:"lock_timeout,omitempty"`
	// MigrationTimeout — клиентский страховочный таймаут миграций (0 — полагаться
	// только на серверный statement_timeout).
	MigrationTimeout Duration `yaml:"migration_timeout,omitempty"`

	// WriteErrorMode задаёт поведение многошардовой записи/миграции при первой
	// ошибке шарда: "stop" (по умолчанию) прерывает остальные и текущие шарды, чтобы
	// ошибочное изменение не разошлось; "continue" применяет ко всем шардам и собирает
	// статус по каждому.
	WriteErrorMode string `yaml:"write_error_mode,omitempty"`

	// AllowInsecureProd отключает блокирующую проверку TLS для прод-кластеров:
	// при false (по умолчанию) прод-кластер, доступный по сети с открытым sslmode
	// (disable/allow/prefer), — фатальная ошибка конфига; true понижает её до
	// предупреждения. Прод-кластеры на loopback разрешены всегда.
	AllowInsecureProd bool `yaml:"allow_insecure_prod,omitempty"`

	// Services отображает имя сервиса (например "item") в его storage'и.
	Services map[string]*Service `yaml:"services"`

	// path — откуда конфиг загружен / куда будет сохранён.
	path string `yaml:"-"`
}

// Service группирует связанные storage'и (кластеры).
type Service struct {
	Storages map[string]*Storage `yaml:"storages"`
}

// Storage описывает один кластер шардов (или одну БД при Count == 1).
//
// HostTemplate и DBTemplate могут содержать плейсхолдеры, раскрываемые по позиции
// шарда p (от 0):
//
//	{p}       -> p          (индекс от 0)
//	{p1}      -> p+1        (индекс от 1)
//	{p:03}    -> p, дополненный нулями до ширины 3
//	{p1:03}   -> p+1, дополненный нулями до ширины 3
type Storage struct {
	HostTemplate string `yaml:"host_template"`
	DBTemplate   string `yaml:"db_template"`
	Port         int    `yaml:"port"`
	User         string `yaml:"user"`
	Password     string `yaml:"password"`
	// Count — число шардов. 1 означает одну (нешардированную) БД.
	Count int `yaml:"count"`
	// MigrationRole, если задана, — роль, до которой записи/миграции этого storage
	// поднимаются через `set local role <role>` (роль с полными правами записи по
	// принципу наименьших привилегий, не суперпользователь). Пусто — роль не задаётся.
	// Это единственный источник роли записи (общего значения на кластер нет), так что
	// роль всегда задаётся явно per-storage.
	MigrationRole string `yaml:"migration_role,omitempty"`
	// SSLMode передаётся в строку подключения (по умолчанию "disable").
	SSLMode string `yaml:"sslmode,omitempty"`
	// Параметры профиля подключения (Feature 14). Все необязательны и
	// подставляются в DSN libpq как есть, когда заданы:
	//   SSLRootCert — корневой CA для verify-ca/verify-full (управляемый CA workflow);
	//   SSLCert/SSLKey — клиентский сертификат и ключ (mTLS);
	//   ConnectTimeout — таймаут установления соединения (в DSN уходит в секундах).
	SSLRootCert    string   `yaml:"sslrootcert,omitempty"`
	SSLCert        string   `yaml:"sslcert,omitempty"`
	SSLKey         string   `yaml:"sslkey,omitempty"`
	ConnectTimeout Duration `yaml:"connect_timeout,omitempty"`
	// PasswordEnv, если задан, — имя переменной окружения, ОТКУДА берётся пароль
	// (секрет не хранится в YAML открытым текстом). Имеет приоритет над Password;
	// разрешается в cluster.Expand.
	PasswordEnv string `yaml:"password_env,omitempty"`
	// PassFile — путь к файлу паролей в формате libpq .pgpass
	// (host:port:db:user:password). Передаётся в DSN как passfile=; libpq берёт из
	// него пароль, когда явный password пуст. Секрет не хранится в YAML открытым
	// текстом. Поддерживает ведущий ~ (домашний каталог).
	PassFile string `yaml:"passfile,omitempty"`
	// Prod помечает прод-кластер. Записи/миграции на проде выполняются под ролью
	// миграции (set role _fa); записи не-прода (staging) роль пропускают.
	Prod bool `yaml:"prod"`
}

// Duration — это time.Duration, сериализуемый в/из человекочитаемой строки
// вроде "30s" или "2m".
type Duration time.Duration

func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Defaults заполняет нулевые поля разумными значениями по умолчанию.
func (c *Config) Defaults() {
	if c.MaxRows == nil {
		v := 1000
		c.MaxRows = &v
	}
	if c.FanoutMode == "" {
		c.FanoutMode = "parallel"
	}
	if c.QueryTimeout == 0 {
		c.QueryTimeout = Duration(30 * time.Second)
	}
	if c.StatementTimeout == "" {
		// Намеренно короткое значение по умолчанию; повышается per-migration через \timeout.
		c.StatementTimeout = "5s"
	}
	// Роль записи не имеет значения по умолчанию и задаётся строго per-storage
	// (Storage.MigrationRole). Незаданная роль означает, что `set role` не выполняется:
	// роль записи включается явно, что делает terox универсальным.
	if c.Services == nil {
		c.Services = map[string]*Service{}
	}
}

// Path возвращает путь файла, к которому привязан конфиг.
func (c *Config) Path() string { return c.path }

// AutoKeymapEnabled сообщает, включена ли конвертация раскладки кириллица→латиница
// (по умолчанию true; отключается только явным auto_keymap: false).
func (c *Config) AutoKeymapEnabled() bool { return c.AutoKeymap == nil || *c.AutoKeymap }

// WriteApproveEnabled сообщает, нужно ли подтверждение перед записью (по
// умолчанию да; отключается явным write_approve: false или командой \write_approve off).
func (c *Config) WriteApproveEnabled() bool { return c.WriteApprove == nil || *c.WriteApprove }

// TimingEnabled — показывать ли длительность запроса по умолчанию (\timing). nil → включено.
func (c *Config) TimingEnabled() bool { return c.Timing == nil || *c.Timing }

// ImpactEnabled — включён ли предпросмотр затрагиваемых строк по умолчанию (\impact). nil → выключено.
func (c *Config) ImpactEnabled() bool { return c.Impact != nil && *c.Impact }

// SuggestEnabled — включена ли inline-автоподсказка по умолчанию (\suggest). nil → включено.
func (c *Config) SuggestEnabled() bool { return c.Suggest == nil || *c.Suggest }

// ExpandedDefault — стартовать ли в развёрнутом (по полю на строку) выводе (\x). nil → выключено.
func (c *Config) ExpandedDefault() bool { return c.Expanded != nil && *c.Expanded }

// ProbeConcurrency — параллелизм для внутренних проб (проверка доступности,
// покрытие каталога, \ping, \doctor): всегда параллельно независимо от fanout_mode,
// так как последовательная проверка живости многих шардов застопорила бы UI.
// Учитывает FanoutConcurrency как необязательное ограничение.
func (c *Config) ProbeConcurrency(n int) int {
	if n < 1 {
		return 1
	}
	if c.FanoutConcurrency > 0 && c.FanoutConcurrency < n {
		return c.FanoutConcurrency
	}
	return n
}

// Concurrency определяет, сколько из n шардов выполняются одновременно. Режим
// sequential → 1 (по одному, по порядку). Режим parallel → все n сразу, если не
// задано ограничение FanoutConcurrency. Всегда возвращает >= 1.
func (c *Config) Concurrency(n int) int {
	if strings.EqualFold(c.FanoutMode, "sequential") {
		return 1
	}
	if c.FanoutConcurrency > 0 && c.FanoutConcurrency < n {
		return c.FanoutConcurrency
	}
	if n < 1 {
		return 1
	}
	return n
}

// StopWritesOnError сообщает, прерывает ли многошардовая запись/миграция остальные
// (и текущие) шарды при первой ошибке шарда. По умолчанию true (stop) — безопасное
// направление для инструмента миграций; write_error_mode: continue применяет ко всем
// шардам и собирает статус по каждому.
func (c *Config) StopWritesOnError() bool {
	return !strings.EqualFold(strings.TrimSpace(c.WriteErrorMode), "continue")
}

// MaxRowsValue определяет лимит строк: явное значение (0 — без ограничения), если
// задано, иначе значение по умолчанию 1000.
func (c *Config) MaxRowsValue() int {
	if c.MaxRows == nil {
		return 1000
	}
	return *c.MaxRows
}

// ServiceNames возвращает имена сервисов в отсортированном порядке.
func (c *Config) ServiceNames() []string {
	names := make([]string, 0, len(c.Services))
	for name := range c.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// StorageNames возвращает имена storage'ей сервиса в отсортированном порядке.
func (c *Config) StorageNames(service string) []string {
	svc, ok := c.Services[service]
	if !ok || svc == nil {
		return nil
	}
	names := make([]string, 0, len(svc.Storages))
	for name := range svc.Storages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DefaultPath возвращает путь конфига по умолчанию, учитывая XDG_CONFIG_HOME.
func DefaultPath() (string, error) {
	if env := os.Getenv("TEROX_CONFIG"); env != "" {
		return env, nil
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "terox", "config.yaml"), nil
}

// ResolvePath выбирает, какой файл конфига загрузить, по порядку: явный путь (-c),
// $TEROX_CONFIG, config.yaml в текущем каталоге, config.yaml рядом с исполняемым
// файлом, затем ~/.config/terox/config.yaml.
func ResolvePath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("TEROX_CONFIG"); env != "" {
		return env, nil
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		if abs, err := filepath.Abs("config.yaml"); err == nil {
			return abs, nil
		}
		return "config.yaml", nil
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "config.yaml")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return DefaultPath()
}

var validatePlaceholderRe = regexp.MustCompile(`\{p1?(?::\d+)?\}`)

// pgDurationRe соответствует грамматике таймаута SET в PostgreSQL (эти строки
// подставляются дословно в `SET statement_timeout = '...'`, а не парсятся как Go
// duration). Единицы регистрозависимы в PostgreSQL; 'off' не принимается (используйте 0/пусто).
var pgDurationRe = regexp.MustCompile(`^(0|[0-9]+(\.[0-9]+)?\s?(us|ms|s|min|h|d)?)$`)

// validatePGDuration проверяет значение по грамматике длительности PostgreSQL.
// Некорректное значение — блокирующая ошибка (errMsg): оно подставляется в
// SET LOCAL statement_timeout при каждом чтении, так что опечатка сломала бы каждый
// запрос — тот же контракт, что обеспечивает migration.BuildTransactional. Субмиллисекундное
// значение — лишь предупреждение (таймаут отключается, но запросы выполняются).
func validatePGDuration(name, val string) (errMsg, warnMsg string) {
	val = strings.TrimSpace(val)
	if val == "" {
		return "", ""
	}
	if !pgDurationRe.MatchString(val) {
		return name + ": " + val + " is not a valid PostgreSQL duration (e.g. 500ms, 5s, 2min)", ""
	}
	// Базовая единица statement_timeout — миллисекунды; значение, округляющееся до 0,
	// молча ОТКЛЮЧАЕТ таймаут. Ловим это для любой единицы (0.5ms, 0.0001s, 500us…),
	// а не только для 'us' (например 2000us = 2ms допустимо и не предупреждается).
	if ms, ok := pgDurationMillis(val); ok && ms > 0 && ms < 1 {
		return "", name + ": '" + val + "' is below 1ms and is rounded to 0 by PostgreSQL (timeout disabled)"
	}
	return "", ""
}

// pgDurationMillis переводит уже валидную (по pgDurationRe) PG-длительность в
// миллисекунды. Голое число трактуется как ms — так PostgreSQL понимает statement_timeout.
func pgDurationMillis(val string) (float64, bool) {
	v := strings.ToLower(strings.TrimSpace(val))
	mult := 1.0 // без единицы — миллисекунды
	for _, u := range []struct {
		suf  string
		mult float64
	}{{"us", 0.001}, {"ms", 1}, {"min", 60000}, {"s", 1000}, {"h", 3600000}, {"d", 86400000}} {
		if strings.HasSuffix(v, u.suf) {
			mult = u.mult
			v = strings.TrimSpace(strings.TrimSuffix(v, u.suf))
			break
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false
	}
	return f * mult, true
}

// insecureSSLModes — значения sslmode libpq, не гарантирующие шифрованный
// аутентифицированный канал: пароль и весь трафик могут идти открыто или быть
// доступны для атаки «человек посередине». "require" шифрует, но не аутентифицирует
// сервер; это делают только verify-ca / verify-full. Пустое — это "disable".
var insecureSSLModes = map[string]bool{
	"": true, "disable": true, "allow": true, "prefer": true,
}

// EffectiveSSLMode возвращает фактически используемый для подключений sslmode,
// применяя то же правило пусто -> "disable", что и слой подключения (cluster.Expand /
// db.dsn), в нижнем регистре.
func (s *Storage) EffectiveSSLMode() string {
	m := strings.ToLower(strings.TrimSpace(s.SSLMode))
	if m == "" {
		return "disable"
	}
	return m
}

// ExpandUserPath разворачивает ведущий ~ или ~/ в домашний каталог. Применяется И при
// валидации (проверка существования файла), И на пути подключения (cluster.Expand),
// чтобы "~/.postgresql/root.crt" в passfile/sslrootcert/sslcert/sslkey указывал на один
// и тот же файл — pgx/libpq сами ~ для ЯВНО заданных путей НЕ разворачивают. Прочие
// ~user не трогаем (редки и требуют lookup); путь возвращается как есть.
func ExpandUserPath(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

// isLoopbackHostTemplate сообщает, является ли шаблон хоста чистым loopback-адресом
// (без плейсхолдеров), где открытый трафик нельзя перехватить по сети — поэтому
// небезопасный sslmode там допускается даже на прод-кластере.
func isLoopbackHostTemplate(template string) bool {
	h := strings.ToLower(strings.TrimSpace(template))
	if h == "localhost" {
		return true
	}
	// Снимаем скобки IPv6-литерала ([::1] → ::1) и проверяем по диапазонам loopback
	// (весь 127.0.0.0/8 и ::1), а не по списку из нескольких точных строк.
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Validate проверяет конфиг на блокирующие ошибки и предупреждения о вероятных
// ошибках настройки. Это тонкая обёртка над Findings(), сохраняющая прежний
// строковый контракт (errs/warns в том же порядке и с теми же сообщениями).
func (c *Config) Validate() (errs, warns []string) {
	return splitFindings(c.Findings())
}

// Findings — структурированная валидация: то же, что Validate(), но каждая находка
// несёт стабильный машиночитаемый Code, Severity и Path (service/storage или пусто).
// Это единый источник истины валидации конфига; строковый Validate() и
// preflight-отчёт строятся поверх него.
func (c *Config) Findings() []Finding {
	var fs []Finding
	addErr := func(code, path, msg string) {
		fs = append(fs, Finding{Code: code, Severity: SeverityError, Path: path, Message: msg})
	}
	addWarn := func(code, path, msg string) {
		fs = append(fs, Finding{Code: code, Severity: SeverityWarning, Path: path, Message: msg})
	}

	if len(c.Services) == 0 {
		addErr("no-services", "", "no services defined")
	}
	for _, sName := range c.ServiceNames() {
		svc := c.Services[sName]
		if svc == nil || len(svc.Storages) == 0 {
			addWarn("service-no-storages", sName, fmt.Sprintf("service %q has no storages", sName))
			continue
		}
		for _, stName := range c.StorageNames(sName) {
			st := svc.Storages[stName]
			id := sName + "/" + stName
			if st == nil {
				// YAML `storage_name:` без тела декодируется в nil *Storage.
				// Сообщаем об этом вместо разыменования (которое вызвало бы панику).
				addErr("storage-empty", id, id+": empty storage definition (expected host_template/user/...)")
				continue
			}
			if strings.TrimSpace(st.HostTemplate) == "" {
				addErr("host-template-empty", id, id+": empty host_template")
			}
			if st.Port <= 0 || st.Port > 65535 {
				addErr("port-invalid", id, fmt.Sprintf("%s: invalid port %d", id, st.Port))
			}
			if strings.TrimSpace(st.User) == "" {
				addWarn("user-empty", id, id+": empty user")
			}
			if st.Count < 0 {
				addErr("count-negative", id, fmt.Sprintf("%s: count must be >= 0, got %d", id, st.Count))
			}
			count := st.Count
			if count <= 0 {
				count = 1
			}
			hostPH := validatePlaceholderRe.MatchString(st.HostTemplate)
			dbPH := validatePlaceholderRe.MatchString(st.DBTemplate)
			if count > 1 && !hostPH && !dbPH {
				addWarn("count-no-placeholder", id, fmt.Sprintf("%s: count=%d but no {p}/{p1} placeholder — every shard resolves to the same host/db", id, count))
			}
			switch {
			case st.PasswordEnv != "":
				// Пароль берётся из переменной окружения — открытый password не важен.
				// Но если переменная пуста/не задана, реальный пароль окажется пустым
				// (env перекрывает plaintext в Expand) — об этом предупреждаем явно.
				if os.Getenv(st.PasswordEnv) == "" {
					addWarn("password-env-empty", id, fmt.Sprintf("%s: password_env=%s but that environment variable is empty/unset — the connection password will be EMPTY", id, st.PasswordEnv))
				}
			case st.Password == "changeme" || (st.Password == "" && strings.TrimSpace(st.PassFile) == ""):
				// Предупреждаем о плейсхолдере 'changeme' (он перекрывает passfile,
				// т.к. явный password имеет приоритет) и о пустом пароле БЕЗ passfile.
				// Пустой password + заданный passfile — штатный безопасный путь
				// (libpq берёт секрет из .pgpass), о нём не предупреждаем; сам файл
				// проверяется ниже находкой profile-file-unreadable.
				addWarn("password-placeholder", id, id+": placeholder/empty password")
			}
			if st.ConnectTimeout < 0 {
				addErr("connect-timeout-negative", id, id+": connect_timeout must not be negative")
			}
			// verify-ca/verify-full без корневого CA = непроверяемая цепочка
			// (управляемого CA workflow нет) — предупреждаем.
			if m := st.EffectiveSSLMode(); (m == "verify-ca" || m == "verify-full") && strings.TrimSpace(st.SSLRootCert) == "" {
				addWarn("ssl-rootcert-missing", id, fmt.Sprintf("%s: sslmode=%s without sslrootcert — relies on the system CA store; set sslrootcert for a managed CA workflow", id, m))
			}
			// Клиентский сертификат и ключ — пара: mTLS требует ОБА (или ни одного).
			// Это ошибка конфигурации, а не предупреждение: libpq не сможет
			// аутентифицироваться сертификатом без ключа (и наоборот).
			if (strings.TrimSpace(st.SSLCert) == "") != (strings.TrimSpace(st.SSLKey) == "") {
				addErr("client-cert-pair", id, id+": sslcert and sslkey must be set together for client-certificate (mTLS) auth")
			}
			// Файлы профиля (CA, клиентский сертификат/ключ, passfile) должны
			// существовать и читаться — иначе libpq упадёт только в момент
			// подключения. Это ПРЕДУПРЕЖДЕНИЕ (не ошибка): конфиг может валидироваться
			// на хосте, где файлов ещё нет (CI), а реально использоваться на целевом.
			for _, f := range []struct{ name, path string }{
				{"sslrootcert", st.SSLRootCert},
				{"sslcert", st.SSLCert},
				{"sslkey", st.SSLKey},
				{"passfile", st.PassFile},
			} {
				p := strings.TrimSpace(f.path)
				if p == "" {
					continue
				}
				// sslrootcert=system — не путь к файлу, а указание брать корневые
				// сертификаты из системного хранилища доверия (pgx это понимает).
				if f.name == "sslrootcert" && strings.EqualFold(p, "system") {
					continue
				}
				resolved := ExpandUserPath(p)
				// Если путь так и остался с ведущим ~ (HOME не задан — бывает в CI/
				// контейнере), развернуть его нельзя, поэтому проверку существования
				// пропускаем: иначе получили бы путающее «not readable» вместо реальной
				// причины (неудача разворачивания ~), а на целевом хосте файл есть.
				if strings.HasPrefix(resolved, "~") {
					continue
				}
				if _, err := os.Stat(resolved); err != nil {
					addWarn("profile-file-unreadable", id, fmt.Sprintf("%s: %s %q is not readable (%v) — the connection will fail until it exists on the target host", id, f.name, p, err))
				}
			}
			// Политика TLS для прод-кластеров. Не-прод (local/staging) не трогаем,
			// чтобы не докучать dev-окружениям, намеренно пропускающим TLS.
			if st.Prod {
				mode := st.EffectiveSSLMode()
				switch {
				case insecureSSLModes[mode]:
					// Открытый текст: пароль и все данные запроса идут незашифрованными.
					// На loopback-хосте их нельзя перехватить по сети; по сети это
					// фатальная ошибка, если allow_insecure_prod не отключает её.
					switch {
					case isLoopbackHostTemplate(st.HostTemplate):
						// допустимо — loopback
					case c.AllowInsecureProd:
						addWarn("prod-insecure-tls-allowed", id, fmt.Sprintf(
							"%s: PROD cluster uses sslmode=%s — password and data travel unencrypted (allowed by allow_insecure_prod)",
							id, mode))
					default:
						addErr("prod-insecure-tls", id, fmt.Sprintf(
							"%s: PROD cluster uses sslmode=%s — password and data travel unencrypted; set sslmode: verify-full (or require), or set allow_insecure_prod: true to override",
							id, mode))
					}
				case mode == "require":
					// Зашифровано, но сертификат сервера не проверяется, поэтому
					// «человек посередине» всё ещё может выдать себя за сервер.
					addWarn("prod-require-unverified", id, fmt.Sprintf(
						"%s: PROD cluster uses sslmode=require — traffic is encrypted but the server certificate is NOT verified (MITM possible); use verify-full with a CA for an authenticated connection",
						id))
				}
			}
		}
	}
	for _, d := range []struct{ name, val string }{
		{"statement_timeout", c.StatementTimeout}, {"lock_timeout", c.LockTimeout},
	} {
		if e, w := validatePGDuration(d.name, d.val); e != "" {
			addErr("pg-duration-invalid", "", e)
		} else if w != "" {
			addWarn("pg-duration-subms", "", w)
		}
	}
	if d := time.Duration(c.QueryTimeout); d > 0 && d < time.Second {
		addWarn("query-timeout-low", "", fmt.Sprintf("query_timeout is very low (%s) — many real queries will time out", d))
	}
	if m := strings.ToLower(strings.TrimSpace(c.FanoutMode)); m != "" && m != "parallel" && m != "sequential" {
		addErr("fanout-mode-invalid", "", fmt.Sprintf("fanout_mode must be 'parallel' or 'sequential', got %q", c.FanoutMode))
	}
	if m := strings.ToLower(strings.TrimSpace(c.WriteErrorMode)); m != "" && m != "stop" && m != "continue" {
		addErr("write-error-mode-invalid", "", fmt.Sprintf("write_error_mode must be 'stop' or 'continue', got %q", c.WriteErrorMode))
	}
	// Проверки числовых диапазонов: отрицательные/абсурдные значения иначе прошли бы
	// незаметно (Defaults заполняет только нулевые значения).
	if c.MaxRows != nil && *c.MaxRows < 0 {
		addErr("max-rows-negative", "", fmt.Sprintf("max_rows must be >= 0 (0 = unlimited), got %d", *c.MaxRows))
	}
	if c.FanoutConcurrency < 0 {
		addErr("fanout-concurrency-negative", "", fmt.Sprintf("fanout_concurrency must be >= 0, got %d", c.FanoutConcurrency))
	}
	if c.QueryTimeout < 0 {
		addErr("query-timeout-negative", "", "query_timeout must not be negative")
	}
	if c.MigrationTimeout < 0 {
		addErr("migration-timeout-negative", "", "migration_timeout must not be negative")
	}
	// Старт в режиме записи против прод-кластера — рискованно: первый же разрушительный
	// оператор отделяет одно подтверждение. Предупреждаем, если задано и то, и другое.
	if c.WriteModeDefault {
		for _, sName := range c.ServiceNames() {
			svc := c.Services[sName]
			if svc == nil {
				continue
			}
			for _, stName := range c.StorageNames(sName) {
				if st := svc.Storages[stName]; st != nil && st.Prod {
					addWarn("write-mode-default-prod", sName+"/"+stName, "write_mode_default: true starts in write mode and there is at least one prod cluster ("+sName+"/"+stName+") — consider starting read-only and enabling writes with \\write on")
					break
				}
			}
		}
	}
	// Конфиг хранит пароли в открытом виде, поэтому не должен быть читаем другими
	// пользователями. Предупреждаем (по возможности), если файл доступен группе/всем.
	if c.path != "" {
		if info, err := os.Stat(c.path); err == nil {
			if perm := info.Mode().Perm(); perm&0o077 != 0 {
				addWarn("config-perms", "", fmt.Sprintf(
					"config file %s is mode %#o — it contains plaintext passwords; tighten it to 0600 (chmod 600 %s)",
					c.path, perm, c.path))
			}
		}
	}
	return fs
}

// Load читает и разбирает конфиг по пути. Если файла нет, возвращается пустой
// (с дефолтами) конфиг, привязанный к пути, с обёрнутой os.ErrNotExist, чтобы
// вызывающий отличал «первый запуск».
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c := &Config{path: path}
			c.Defaults()
			return c, err
		}
		return nil, err
	}
	var c Config
	// Строгий декод: неизвестный/опечатанный ключ (например `ssl_mode:` вместо
	// `sslmode:` или `passwrd:`) — фатальная ошибка, а не молчаливое игнорирование:
	// опечатка в настройке безопасности не должна сойти за дефолт.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.path = path
	c.Defaults()
	return &c, nil
}

// Save записывает конфиг по привязанному пути с правами 0600, создавая родительский
// каталог при необходимости.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config has no path set")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return err
	}
	_ = enc.Close()
	data := buf.Bytes()
	// Сохраняем написанные вручную комментарии: копируем их из YAML-дерева
	// существующего файла в свежесериализованное (чтобы \editor / \add не стирали
	// комментарии пользователя). По возможности — при любом сбое откатываемся к
	// обычному выводу.
	if old, err := os.ReadFile(c.path); err == nil {
		if merged, err := mergeYAMLComments(old, data); err == nil {
			data = merged
		}
	}
	// Атомарная запись через уникальный временный файл в том же каталоге, затем
	// rename. Уникальное имя (не предсказуемое "<config>.tmp") избегает гонки с
	// symlink/перезаписью, а явный Chmod гарантирует 0600, даже если файл с таким
	// именем уже существовал с более широкими правами (os.WriteFile не сузил бы права
	// существующего файла). Временный файл удаляется при любой ошибке, чтобы
	// недописанный секрет не остался.
	dir := filepath.Dir(c.path)
	f, err := os.CreateTemp(dir, ".terox-config-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if err := f.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, c.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// mergeYAMLComments копирует head/line/foot-комментарии из старого YAML-документа
// в новый, рекурсивно сопоставляя ключи map. Ключи только в новом документе остаются
// без комментариев; ключи только в старом отбрасываются вместе с комментариями.
// Возвращает переэнкоженный (2 пробела) документ.
func mergeYAMLComments(oldBytes, newBytes []byte) ([]byte, error) {
	var oldDoc, newDoc yaml.Node
	if err := yaml.Unmarshal(oldBytes, &oldDoc); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(newBytes, &newDoc); err != nil {
		return nil, err
	}
	if len(oldDoc.Content) == 0 || len(newDoc.Content) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	copyYAMLComments(oldDoc.Content[0], newDoc.Content[0])
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&newDoc); err != nil {
		return nil, err
	}
	_ = enc.Close()
	return buf.Bytes(), nil
}

// copyYAMLComments переносит комментарии из old в new для совпадающих ключей
// mapping, рекурсивно заходя во вложенные mapping'и.
func copyYAMLComments(old, new *yaml.Node) {
	if old == nil || new == nil || old.Kind != new.Kind {
		return
	}
	new.HeadComment = orStr(new.HeadComment, old.HeadComment)
	new.LineComment = orStr(new.LineComment, old.LineComment)
	new.FootComment = orStr(new.FootComment, old.FootComment)
	if new.Kind != yaml.MappingNode {
		return
	}
	oldKeyNode := map[string]*yaml.Node{}
	oldValNode := map[string]*yaml.Node{}
	for i := 0; i+1 < len(old.Content); i += 2 {
		oldKeyNode[old.Content[i].Value] = old.Content[i]
		oldValNode[old.Content[i].Value] = old.Content[i+1]
	}
	for i := 0; i+1 < len(new.Content); i += 2 {
		k := new.Content[i].Value
		if okn, ok := oldKeyNode[k]; ok {
			new.Content[i].HeadComment = orStr(new.Content[i].HeadComment, okn.HeadComment)
			new.Content[i].LineComment = orStr(new.Content[i].LineComment, okn.LineComment)
			new.Content[i].FootComment = orStr(new.Content[i].FootComment, okn.FootComment)
			copyYAMLComments(oldValNode[k], new.Content[i+1])
		}
	}
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// IsEmpty сообщает, не определено ли в конфиге ни одного сервиса.
func (c *Config) IsEmpty() bool {
	for _, svc := range c.Services {
		if svc != nil && len(svc.Storages) > 0 {
			return false
		}
	}
	return true
}
