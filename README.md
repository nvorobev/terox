# terox — интерактивный мультишардовый клиент PostgreSQL

`terox` — CLI-клиент для десятков шардированных кластеров PostgreSQL. Выбор сервиса →
хранилища → шарда (один, подмножество или все); запросы выполняются с историей, а
результат со всех шардов сводится в одну таблицу. По умолчанию работает в **read-only**;
записи и миграции включаются явно и подтверждаются, а при настроенной `migration_role`
выполняются под ней.

## Содержание

- [Установка](#установка)
- [Конфигурация](#конфигурация)
- [Запуск и подкоманды](#запуск-и-подкоманды)
- [Контекст и переключение целей](#контекст-и-переключение-целей)
- [Чтение запросов](#чтение-запросов)
- [Записи и миграции](#записи-и-миграции)
- [Справочник команд](#справочник-команд)
- [Типовые сценарии](#типовые-сценарии)

---

## Установка

Нужна установленная Go-тулчейн (Go 1.25+) — сборка идёт через `make`/`go build`.
Целевые БД — PostgreSQL 13+.

```sh
cd terox
make            # чистая переустановка: снести старые копии → пересобрать →
                # поставить одну свежую на PATH (+ конфиг рядом) → проверить версию
terox           # запуск
```

- `make build` — только собрать `./terox` в каталоге проекта (без установки).
- `make install` — собрать и поставить бинарь + `config.yaml` на PATH (без полной
  переустановки как у `make`; sudo только если каталог не writable).
- `make install-go` — без sudo, в `$(go env GOPATH)/bin` (должен быть в PATH).
- `make install PREFIX=$HOME/.local` (или `BINDIR=<dir>`) — другой каталог установки.
- `make uninstall` — снести все копии `terox` на PATH / в известных каталогах.
- `make help` — все цели; `terox version` — версия сборки.

Вручную: `go build -o terox .` или `go install .`.

---

## Конфигурация

### Где лежит конфиг

Порядок поиска (берётся первый найденный):

1. `-c <путь>`;
2. `$TEROX_CONFIG`;
3. `./config.yaml`;
4. `config.yaml` рядом с бинарём;
5. `~/.config/terox/config.yaml` (или `$XDG_CONFIG_HOME/terox/config.yaml`).

Проверка файла: `terox validate` (`--json` для CI, `--strict` — предупреждения как ошибки).
Конфиг хранится вне git — там пароли.

### Обязательные поля хранилища (storage)

| Поле | Назначение |
|---|---|
| `host_template` | шаблон хоста; плейсхолдеры `{p}`/`{p1}` для номера шарда |
| `db_template` | шаблон имени БД; плейсхолдеры допустимы |
| `port` | порт PgBouncer |
| `user` | пользователь подключения (минимально привилегированный, read-only) |
| `password` | пароль |
| `count` | число шардов (`1` = одиночная БД) |
| `sslmode` | `disable\|allow\|prefer\|require\|verify-ca\|verify-full` (не задан → `disable`) |
| `prod` | `true` = production (красный бейдж + усиленные подтверждения записи) |
| `migration_role` | роль, под которой идут записи (`set local role`); не задано → без `set role` |

### Режимы TLS (`sslmode`)

Значения совпадают с libpq и идут по нарастанию строгости:

| Режим | Шифрование | Проверка сертификата сервера |
|---|---|---|
| `disable` | нет | — |
| `allow` | только если сервер сам потребует | нет |
| `prefer` | если сервер поддерживает (по умолчанию у libpq) | нет |
| `require` | да | нет — от MITM не защищает |
| `verify-ca` | да | подпись доверенным CA (`sslrootcert`) |
| `verify-full` | да | подпись CA **и** совпадение hostname с сертификатом |

От подмены сервера (MITM) защищает только `verify-full` (и частично `verify-ca`):
`require` шифрует канал, но не проверяет, что на том конце именно ваш сервер. Для prod
рекомендуется `verify-full` с указанным `sslrootcert`. На сетевом prod нешифрованные
режимы (`disable`/`allow`/`prefer`) запрещены, если не выставлен `allow_insecure_prod: true`.

### Плейсхолдеры шарда

Подставляются по 0-based позиции шарда `p` (0..count-1):

| Плейсхолдер | Значение | Пример |
|---|---|---|
| `{p}` | `p` (0-based) | `shard_{p}` → `shard_0` |
| `{p1}` | `p+1` (1-based) | `rs{p1}` → `rs1` |
| `{p:03}` | `p`, ширина 3 | `rs{p:03}` → `rs000` |
| `{p1:03}` | `p+1`, ширина 3 | `rs{p1:03}` → `rs001` |

Можно по-разному в host и db: `host_template: rs{p1:02}` + `db_template: shard_{p}` →
`rs01/shard_0 … rs32/shard_31`.

### Необязательные поля хранилища

`sslrootcert` (CA для verify-ca/verify-full), `sslcert`/`sslkey` (mTLS-пара),
`connect_timeout`, `passfile` (libpq `.pgpass`), `password_env: VAR` (пароль из переменной
окружения вместо открытого в YAML).

### Пример

```yaml
write_mode_default: false   # стартовать в read-only
editor: tea                 # редактор ввода: tea (живая выпадашка) | readline
auto_keymap: true           # авто-конверсия русской раскладки (ЙЦУКЕН→QWERTY)
max_rows: 100               # лимит строк на вывод (0 = без лимита)
# timing: true              # показывать длительность запроса (\timing; по умолч. вкл.)
# impact: false             # превью затрагиваемых строк перед UPDATE/DELETE (\impact; по умолч. выкл.)
# suggest: true             # inline ghost-автоподсказка (\suggest; по умолч. вкл.)
# expanded: false           # стартовать в развёрнутом выводе (\x; по умолч. выкл.)
fanout_mode: parallel       # запросы на все шарды: parallel | sequential
query_timeout: 5s           # клиентский таймаут на запрос (на каждый шард)
statement_timeout: 1s       # серверный statement_timeout для чтений и миграций (\timeout меняет)
# lock_timeout: 500ms       # серверный lock_timeout для миграций (необязательно)
# migration_timeout: 30m    # клиентский дедлайн миграции; ОБЯЗАТЕЛЕН для CONCURRENTLY/VACUUM на prod
# write_error_mode: stop    # запись на все шарды: stop | continue
# write_approve: true       # подтверждение перед записью (\write_approve off — отключить)
# allow_insecure_prod: false  # разрешить нешифрованный sslmode на сетевом prod (иначе ошибка)

services:
  shop:                                # сервис
    storages:
      sharded:                         # хранилище (кластер)
        host_template: db-shop-rs{p1:03}.internal   # rs001 … rs128
        db_template: master
        port: 6432
        user: app_ro
        password: secret
        count: 128
        sslmode: verify-full
        prod: true
        migration_role: app_writer     # writes: set local role app_writer
```

### Минимальный пример для одиночной (нешардовой) БД

При `count: 1` плейсхолдеры не нужны — в `host_template`/`db_template` указываются готовые
имена хоста и базы как есть:

```yaml
services:
  app:
    storages:
      local:
        host_template: 127.0.0.1   # обычный хост, без {p}/{p1}
        db_template: app_db        # обычное имя БД
        port: 6432
        user: app_ro
        password: secret
        count: 1
```

Контекст для такой БД: `terox -t app/local` (селектор шарда не нужен).

---

## Запуск и подкоманды

```sh
terox                          # интерактивный REPL, выбор контекста стрелками
terox -t shop/sharded/all      # сразу в контекст, без меню
terox -t shop/sharded/0,1,5    # подмножество шардов
terox -c ./my.yaml -t shop/... # другой конфиг
terox add                      # мастер регистрации кластера
terox validate                 # проверить конфиг (--json для CI; --strict — warnings → errors)
terox version                  # версия (также --version, -v)
terox help                     # справка по запуску

# Неинтерактивно (для скриптов/CI):
terox query -t shop/sharded/all --format json "select ..."        # read-only; format table|json|csv|envelope
terox query -t shop/sharded/all --order-by id:desc "select ..."   # глобальная сортировка по шардам
terox query -t shop/sharded/all --mode aggregate "select count(*)"
terox plan  -t shop/sharded/all --analyze "select ..."            # анализ плана в JSON
terox migrate -t shop/sharded/all --canary <file.sql>             # offline-превью payload+плана раскатки (без БД)
```

Ключевые флаги: `-c PATH`, `-t SPEC` (`service/storage[/selector]`), `--format`
(table|json|csv|envelope для `query`), `--order-by COL[:asc|:desc]`, `--mode`
(union|strict|merge-sort|quorum|aggregate|first-success|per-shard), `--analyze` (для
`plan`), `--strict` (предупреждения конфига блокируют), `--allow-warning CODE`.

`terox migrate` — только offline-превью (без подключения и без применения); реальный
накат остаётся за интерактивным `\migrate --allowed`.

**SQL из stdin для `terox query`.** Если позиционный SQL не задан и stdin не терминал
(пайп или heredoc), запрос читается из stdin. Удобно для CI/Makefile без экранирования
кавычек и `$`:

```sh
echo 'select count(*) from items' | terox query -t shop/sharded/all --format json
terox query -t test/local <<'SQL'
  select * from items
  where status = 'new';
SQL
```

**Коды возврата `terox query`:** `0` — все шарды ответили; `2` — частичный успех
(результат неполный); `1` — полный провал / ошибка конфига или использования.

При ошибке в конфиге terox не стартует (код 1); `help`/`version` конфиг не требуют.

Подкоманды `query`/`plan`/`migrate` используют **тот же конфиг и тот же поиск пароля**,
что и REPL (см. раздел «Конфигурация»): отдельно ничего передавать не нужно, при желании
другой файл — через `-c PATH`.

---

## Контекст и переключение целей

**Контекст** = `сервис / хранилище (шард|all|подмножество)`. Виден в промпте:

```
shop/sharded(rs02/shard_1) [prod ro st=5s] =>
```

— сервис/хранилище(цель) · окружение (`prod`/staging) · режим (`ro`/`wr`) · текущий
`statement_timeout`.

Без `-t` контекст выбирается в меню стрелками: сервис → хранилище → шард (`← back`,
`Esc` — шаг вверх / выход). На шаге шарда: `all`, custom (`0,1,3..7`) или конкретный шард.

Переключение в сессии:

| Команда | Назначение |
|---|---|
| `\use [service]` | сменить сервис (меню) |
| `\c <storage> [selector]` | сменить хранилище; selector = label / `all` / `0,1,3..7` |
| `\shard [selector]` (`\s`) | переключить шарды в текущем хранилище (меню без аргумента) |
| `\shards` | список шардов и какие выбраны |
| `\l` | список сервисов и хранилищ |
| `\add` | мастер регистрации нового кластера |

**Селектор шардов:** `all`; `rs005`/`shard_3` (по label); `3` (по индексу); `0,1,3..7,10`
(список и диапазоны). `..` — диапазон включительно по индексам, т.е. `3..7` = `3,4,5,6,7`.
Быстрый сценарий: запрос на `all` → колонка `shard` показывает, где данные → `\s <label>`
мгновенно переключает на нужный шард.

---

## Чтение запросов

- Многострочный ввод, выполнение по `;` (как в psql). В редакторе `tea` оператор
  редактируется прямо в несколько строк: `Enter` добавляет перенос, пока нет
  завершающей `;`, а `↑`/`↓` ходят по строкам буфера (на краях — по истории). Для
  правки большого запроса целиком — `\e` (внешний `$EDITOR`, предзаполнен последним).
- **Один шард** → обычная таблица. **Несколько** → объединённая таблица с колонкой
  `shard`; недостающие колонки — `NULL`; ошибочные шарды — отдельным блоком внизу.
- Чтение всегда идёт в `READ ONLY`-транзакции.

```
\x                 вертикальный (record-per-line) вывод для широких строк
\maxrows [N]       лимит строк на вывод (\maxrows unlimited — снять)
\timing [on|off]   показ длительности запроса
\g [selector]      повтор последнего запроса на текущих целях
\gx [selector]     то же в expanded-выводе
```

**Повтор последнего запроса:** `\g` повторяет последний запрос на текущих целях; `\gx`
— то же в expanded-выводе. Необязательный селектор шардов (`\g rs042`, `\gx 0,1`)
повторяет запрос только на этом подмножестве, не меняя остальной контекст. Если последним
был пишущий запрос, повтор заново про-гейчивается (режим записи + подтверждение), а не
выполняется молча.

---

## Записи и миграции

**Read-only по умолчанию.** Любой пишущий запрос отклоняется с подсказкой `\write on`.
Режим записи — это per-context lease: сбрасывается при смене хранилища/сервиса.

```
\write on                запись разрешена (плюс подтверждение перед каждой)
\write_approve off       отключить подтверждение записи
\impact on               включить превью затрагиваемых строк перед UPDATE/DELETE
```

- **Превью влияния:** по умолчанию выключено; включается через `\impact on`. После включения
  перед `UPDATE`/`DELETE` показывается, сколько строк затронет на каждом шарде, затем
  спрашивается подтверждение. `UPDATE … FROM`/`DELETE … USING` превью не строят.
- `UPDATE`/`DELETE` без `WHERE` и `TRUNCATE` требуют усиленного подтверждения.
- **`DROP DATABASE` запрещён безусловно** — отклоняется на любом пути (запись, `\migrate`,
  staged-rollout, `\i`), даже в write-режиме с подтверждением: он необратимо уничтожает всю
  базу шарда. Если это действительно нужно — сделай вне terox (psql/админ-тулинг).
- Записи идут под `set local role <migration_role>` — только если она задана у хранилища.
  Если `migration_role` не задана, запись идёт под `user` хранилища (тем, кто указан в
  конфиге для подключения) — у него должны быть права на запись.

**Первая запись «с нуля»:** старт всегда в read-only.
```
\write on                            # включить режим записи (per-context lease)
update items set status='ok' where id=1;   # превью затронутых строк → подтверждение → пер-шард статус
```
`migration_role` — это существующая роль в самой БД (создаётся DBA/администратором БД,
terox её не создаёт); если она прописана в конфиге хранилища, terox делает `set local
role` перед каждой записью.

### `\migrate` — обёртка (рекомендуется)

```
\migrate [--allowed] [--check] [--canary|--batch N|--resume] [--force] <file.sql>
```

Передаётся **тело** миграции (без своих `begin/commit` и `set role`). terox сам оборачивает
его в одну транзакцию с `set local role` + `statement_timeout`.

- **Dry-run по умолчанию:** без `--allowed` печатает точный exec, план раскатки и
  migration-aware lint, ничего не применяя. `--allowed` — реально применить на текущий
  таргет (обычно `all`).
- **`--check`** — только статический lint без БД: помечает `ADD COLUMN NOT NULL` без
  `DEFAULT`, `CREATE INDEX` без `CONCURRENTLY`, `ADD FK`/`CHECK` без `NOT VALID`,
  переписывающий таблицу `ALTER … TYPE`, `DROP` без `IF EXISTS`. Эвристика.
- Раскатка: `--canary` — сначала один шард; `--batch N` — батчами с барьером; `--resume`
  — донакатить только не-применённые шарды (по локальному журналу).
- Дрейф контрольной суммы файла блокируется без `--force`.

### `\i` — pass-through (для экспертов)

```
\i [--allowed] <file.sql>
```

Шлёт файл **дословно** одним exec — `begin/commit`, `set role`, таймауты задаются вручную.
Тоже dry-run по умолчанию.

### Нетранзакционные операции

`CREATE/DROP/REINDEX … CONCURRENTLY`, `VACUUM`, `CLUSTER`, `ALTER SYSTEM` и т.п. нельзя
в транзакции — terox их авто-определяет и выполняет отдельными autocommit-exec'ами без
обёртки. На **prod** для них **обязателен** ненулевой `migration_timeout` в конфиге.

---

## Справочник команд

Полный список мета-команд (из `\help`). ⚠ помечены команды, которые могут писать в БД.
Синтаксис дословный; короткие алиасы указаны в скобках.

`\help <keyword>` ищет по справке, если точная команда не найдена (подстрока в имени или
назначении): `\help lock` → связанные команды. При неизвестной мета-команде (опечатке)
предлагаются ближайшие команды (did-you-mean).

### Navigation — навигация по целям

| Команда | Синтаксис | Назначение |
|---|---|---|
| `\use` | `\use [service]` | сменить сервис (интерактивное меню service→storage→shard) |
| `\c` (`\connect`) | `\c <storage> [selector]` | сменить хранилище; selector = label / `all` / `0,1,3..7` |
| `\shard` (`\s`) | `\shard [selector]` | переключить шарды в текущем хранилище (меню без аргумента) |
| `\shards` | `\shards` | список шардов и какие выбраны |
| `\l` | `\l` | список сервисов и хранилищ |
| `\add` | `\add` | мастер регистрации нового кластера |

### Modes & input — режимы и ввод

| Команда | Синтаксис | Назначение |
|---|---|---|
| `\write` ⚠ | `\write on\|off` | режим записи (read-only по умолчанию; сбрасывается при смене контекста) |
| `\write_approve` ⚠ | `\write_approve [on\|off]` | подтверждение перед записью (on по умолчанию) |
| `\timeout` | `\timeout [value\|off]` | серверный `statement_timeout` (виден как `st=` в промпте) |
| `\maxrows` | `\maxrows [N\|unlimited]` | лимит отображаемых строк на запрос |
| `\timing` | `\timing [on\|off]` | показ длительности запроса (on по умолчанию) |
| `\x` | `\x` | развёрнутый (одно поле на строку) вывод |
| `\impact` | `\impact [on\|off]` | превью затрагиваемых строк перед записью (off по умолчанию) |
| `\suggest` | `\suggest [on\|off]` | inline ghost-автоподсказка (on по умолчанию) |
| `\editor` | `\editor [tea\|readline]` | выбор редактора строки (меню без аргумента); сохраняется в конфиг |
| `\layout` | `\layout on\|off` | авто-конверсия кириллицы в латиницу (ЙЦУКЕН→QWERTY); сохраняется в конфиг |
| `\e` (`\edit`) | `\e` | редактировать многострочный запрос в `$EDITOR`; сохранить+выйти → выполнить |

### Diagnostics — диагностика

| Команда | Синтаксис | Назначение |
|---|---|---|
| `\ping` | `\ping` | доступность и латентность всех целевых шардов |
| `\doctor` | `\doctor [--all]` | health-check (соединения, локи, bloat, невалидные индексы, wraparound…); `--all` агрегирует по всем шардам |
| `\diff` | `\diff <table>` | дрейф схемы таблицы между шардами: колонки, индексы (вкл. INVALID), ограничения (вкл. NOT VALID), триггеры, RLS, партиции, view |
| `\compare` | `\compare <service/storage>` | сравнить схему, индексы, версии расширений и конфиг с другим хранилищем |
| `\completion` | `\completion [status\|reload]` | состояние каталога автодополнения или его перезагрузка |
| `\activity` | `\activity [--all] [--raw]` | живые backend'ы (pg_stat_activity); `--all` включает idle, `--raw` — литералы без маскировки |
| `\blockers` | `\blockers [--raw]` | заблокированные backend'ы и кто их блокирует (pg_blocking_pids) |
| `\locks` | `\locks` | сводка блокировок по шардам |
| `\longtx` | `\longtx [duration] [--raw]` | транзакции (вкл. idle-in-transaction) старше порога (по умолчанию 1m) |
| `\sizes` | `\sizes [N]` | топ-N таблиц по полному размеру (куча + индексы + TOAST) и доле мёртвых строк, по шардам; ведущая колонка shard показывает межшардовый перекос (по умолчанию 20) |
| `\statements` (`\workload`) | `\statements [N\|snapshot\|diff] [--mean\|--calls\|--rows\|--max] [--user U] [--db D] [--queryid Q] [--skew]` | top-N запросов из pg_stat_statements; `--skew` — перекос queryid по шардам; `snapshot`/`diff` — поиск регрессий |
| `\cancel` ⚠ | `\cancel <pid>` | отменить запрос backend'а (pg_cancel_backend) на одном выбранном шарде |
| `\terminate` ⚠ | `\terminate <pid>` | оборвать backend (pg_terminate_backend) на одном шарде (с подтверждением) |

### Data — данные

| Команда | Синтаксис | Назначение |
|---|---|---|
| `\count` | `\count <table> [where]` | счётчики по шардам + суммарный TOTAL |
| `\locate` (`\find`) | `\locate <table> [where]` | найти шарды с подходящими строками (авто-прыжок, если один) |
| `\dt` | `\dt` | список пользовательских таблиц |
| `\dn` | `\dn` | список пользовательских схем |
| `\di` | `\di` | список индексов пользовательских таблиц с размером |
| `\d` | `\d [table]` | без аргумента — список таблиц; с таблицей — её описание на первом шарде: колонки, индексы, внешние ключи, что на неё ссылается, check-ограничения, размер (для межшардового дрейфа — `\diff`) |
| `\g` (`\gx`) | `\g [selector]` / `\gx [selector]` | повтор последнего запроса на текущих целях (`\gx` — expanded); селектор повторяет только на подмножестве шардов; запись заново про-гейчивается |
| `\grep` | `\grep [-v] <pattern>` | фильтр последнего результата в памяти по подстроке (без перезапроса); `-v` — строки без совпадения |
| `\export` | `\export csv\|json <file>` | записать последний результат в файл |
| `\copy` ⚠ | `\copy <table\|(query)> to <file> [csv\|text\|tsv]` / `\copy <table> from <file> [csv\|tsv]` | клиентский COPY: выгрузка в файл / загрузка из файла (один шард; `from` требует `\write on`) |
| `\watch` | `\watch [interval] <query \| \diag-command>` | повтор read-запроса ИЛИ диагностической команды (`\activity`/`\blockers`/`\locks`/`\longtx`/`\statements`) по интервалу (`500ms`/`5s`/`2m` или голые секунды; по умолчанию `2s`) |

### Plans — планы запросов

| Команда | Синтаксис | Назначение |
|---|---|---|
| `\advise` | `\advise <query>` | index advisor: EXPLAIN (без выполнения) + предложение индекса с rollback (эвристика) |
| `\lint` | `\lint <sql>` | статическая диагностика без БД: unqualified write, LIMIT без ORDER BY, NOT IN с nullable, SELECT *, декартово произведение |
| `\explain` ⚠ | `\explain [analyze] [--memory\|--serialize\|--generic-plan] [--all\|--first\|--shard L\|--sample N\|--outliers] <query>` | разбор плана с drill-down худших оценок; `analyze` ВЫПОЛНЯЕТ запрос (только чтение); подкоманды `save`/`compare`/`diff`/`-f` |

### Files & migrations — файлы и миграции

| Команда | Синтаксис | Назначение |
|---|---|---|
| `\migrate` (`\m`) ⚠ | `\migrate [--allowed] [--check] [--canary\|--batch N\|--resume] [--force] <file.sql>` | миграция-тело в обёртке (одна транзакция, роль, таймауты, один exec); **dry-run по умолчанию** (превью + migration-aware lint), `--allowed` — применить, `--check` — только статический lint без БД |
| `\i` (`\include`) ⚠ | `\i [--allowed] <file.sql>` | файл дословно одним exec (begin/commit и роль — вручную); dry-run по умолчанию |

### Saved queries — сохранённые запросы

| Команда | Синтаксис | Назначение |
|---|---|---|
| `\save` | `\save <name> [sql]` | сохранить запрос (последний, если sql опущен); `:name` — параметры |
| `\run` | `\run <name>` | выполнить сохранённый запрос, спросив `:name`-параметры |
| `\queries` | `\queries` | список сохранённых запросов |
| `\unsave` | `\unsave <name>` | удалить сохранённый запрос |

### History & misc — история и прочее

| Команда | Синтаксис | Назначение |
|---|---|---|
| `\h` (`\history`) | `\h` | подсказка по истории (↑/↓ листать, Ctrl-R поиск) |
| `\help` (`\?`) | `\help [command]` | справка; с аргументом — синтаксис, примеры и риски команды. Если точной команды нет, идёт поиск по справке (подстрока в имени, синтаксисе или назначении): `\help lock` → связанные команды (`\doctor`, `\blockers`, `\locks`, `\watch`, `\migrate`) |
| `\q` (`\quit`) | `\q` | выход |

---

## Типовые сценарии

Все примеры ниже сняты на реальном двухшардовом стенде (`shop/sharded` = `shard_0` +
`shard_1`, таблицы `items`/`users`). Вывод настоящий, не выдуман.

### Чтение и поиск данных

Запрос идёт на все шарды; результат сводится в одну таблицу с ведущей колонкой `shard`:

```
shop/sharded(all) => select item_id, status, price from items where item_id in (1, 200) order by item_id;
┌─────────┬─────────┬────────┬───────┐
│ shard   │ item_id │ status │ price │
├─────────┼─────────┼────────┼───────┤
│ shard_0 │ 1       │ paid   │ 1.5   │
│ shard_1 │ 200     │ new    │ 300   │
└─────────┴─────────┴────────┴───────┘
(2 rows across 2 shards)
```

`\count` суммирует по шардам, `\locate` находит шард с данными и сам прыгает на него,
если он один:

```
shop/sharded(all) => \count items status = 'paid'
┌─────────┬───────┐
│ shard   │ count │
├─────────┼───────┤
│ shard_0 │ 38    │
│ shard_1 │ 37    │
└─────────┴───────┘
TOTAL: 75 across 2 shard(s)

shop/sharded(all) => \locate items item_id = 200
┌─────────┬───────┐
│ shard   │ count │
├─────────┼───────┤
│ shard_1 │ 1     │
└─────────┴───────┘
found on 1/2 shard(s)
→ shop/sharded(shard_1)
```

`\grep` фильтрует последний результат в памяти (без перезапроса), `\x` — развёрнутый вывод:

```
shop/sharded(all) => select item_id, status, price from items order by item_id limit 4;
shop/sharded(all) => \grep paid
┌─────────┬─────────┬────────┬───────┐
│ shard   │ item_id │ status │ price │
├─────────┼─────────┼────────┼───────┤
│ shard_0 │ 1       │ paid   │ 1.5   │
│ shard_1 │ 153     │ paid   │ 229.5 │
└─────────┴─────────┴────────┴───────┘
2 of 8 rows match
```

### Инспекция схемы

`\d <table>` описывает таблицу на первом шарде (колонки, индексы, размер); `\diff` сверяет
схему между всеми шардами:

```
shop/sharded(0) => \d items
items

Columns
┌────────────┬──────────────────────────┬──────────┬─────────┐
│ column     │ type                     │ nullable │ default │
├────────────┼──────────────────────────┼──────────┼─────────┤
│ id         │ bigint                   │ no       │ NULL    │
│ item_id    │ integer                  │ no       │ NULL    │
│ status     │ text                     │ no       │ NULL    │
│ price      │ numeric(10,2)            │ no       │ NULL    │
│ created_at │ timestamp with time zone │ no       │ now()   │
└────────────┴──────────────────────────┴──────────┴─────────┘

shop/sharded(all) => \diff items
all 2 shard(s) identical for items
  columns:     id bigint NOT NULL, item_id integer NOT NULL, status text NOT NULL, …
  indexes:     CREATE UNIQUE INDEX items_pkey … | CREATE INDEX items_status_idx …
  constraints: PRIMARY KEY (id)
```

### Диагностика

```
shop/sharded(all) => \ping
┌─────────┬────────┬─────────┐
│ shard   │ status │ latency │
├─────────┼────────┼─────────┤
│ shard_0 │ OK     │ 6ms     │
│ shard_1 │ OK     │ 2ms     │
└─────────┴────────┴─────────┘
2/2 reachable

shop/sharded(0) => \doctor
doctor [shard_0]   status: OK   (CRITICAL 0, WARNING 0, INFO 1)
  [INFO] Idle-in-transaction connections present
     1 connection(s) idle in transaction — they can block autovacuum and hold locks

shop/sharded(all) => \sizes
┌─────────┬──────────────┬────────────┬──────────────┬──────────┐
│ shard   │ table        │ total_size │ indexes_size │ dead_pct │
├─────────┼──────────────┼────────────┼──────────────┼──────────┤
│ shard_0 │ public.items │ 80 kB      │ 32 kB        │ 0.0%     │
│ shard_1 │ public.items │ 80 kB      │ 32 kB        │ 0.0%     │
└─────────┴──────────────┴────────────┴──────────────┴──────────┘
```

Команды, требующие расширения/прав, отвечают понятной подсказкой, а не падают:

```
shop/sharded(0) => \statements
pg_stat_statements is not available on the selected shard(s).
enable it: add 'pg_stat_statements' to shared_preload_libraries (needs restart), then CREATE EXTENSION pg_stat_statements;
```

### Планы и статический анализ

`\lint` ловит проблемы без БД; `\explain` разбирает план (без `analyze` — только оценка,
ничего не выполняя):

```
shop/sharded(all) => \lint select * from items, users
  [info/low] SELECT * fetches every column and is fragile to schema changes
  [warning/medium] comma-separated FROM tables with no WHERE/JOIN condition — likely a Cartesian product
     fix: add a join condition (WHERE a.x = b.y or an explicit JOIN ... ON)

shop/sharded(0) => \explain select * from items where status = 'paid'
EXPLAIN summary [shard_0]
  estimated plan (not executed); planning: 0.0 ms
  costliest estimated branch: Seq Scan on items
  risk: unknown (estimate only — run \explain analyze for a measured diagnosis)
```

### Запись с превью влияния

Перед `UPDATE`/`DELETE` terox показывает, сколько строк затронет на каждом шарде, и ждёт
подтверждения; затем — пер-шардовый статус:

```
shop/sharded(all) [wr] => update items set status = 'done' where item_id = 200;
impact preview — rows that match
┌─────────┬──────────────┐
│ shard   │ would_affect │
├─────────┼──────────────┤
│ shard_0 │ 0            │
│ shard_1 │ 1            │
└─────────┴──────────────┘
TOTAL: 1 rows would be affected
apply on 2 shard(s)? [y/N] y
┌─────────┬────────┬──────┬──────┐
│ shard   │ status │ rows │ time │
├─────────┼────────┼──────┼──────┤
│ shard_0 │ OK     │ 0    │ 0ms  │
│ shard_1 │ OK     │ 1    │ 1ms  │
└─────────┴────────┴──────┴──────┘
summary: 2 OK, 0 failed (of 2)
```

### Миграции: ввести → посмотреть превью → применить

`\migrate <file>` без `--allowed` — это **dry-run**: показывает ТОЧНЫЙ exec, который уйдёт
на каждый шард (тело в обёртке `begin/set local/commit`), и migration-aware lint. Применение —
отдельной командой с `--allowed`:

```
shop/sharded(all) [wr] => \migrate add_note.sql
-- exact exec terox sends to each shard --
begin;
set local statement_timeout = '5s';

ALTER TABLE items ADD COLUMN IF NOT EXISTS note text;

commit;
migration lint: no risky online-migration patterns (heuristic, no DB)
— dry-run (default). Re-run with --allowed to actually apply. —

shop/sharded(all) [wr] => \migrate --allowed add_note.sql
migration add_note.sql → 2 shard(s) [all] as one exec, role=(none) statement_timeout=5s
┌─────────┬────────┬──────┬──────┐
│ shard   │ status │ rows │ time │
├─────────┼────────┼──────┼──────┤
│ shard_0 │ OK     │ 0    │ 5ms  │
│ shard_1 │ OK     │ 0    │ 2ms  │
└─────────┴────────┴──────┴──────┘
summary: 2 OK, 0 failed (of 2)
```

`\migrate --check <file>` — только статический lint опасных онлайн-паттернов (без БД):

```
shop/sharded(all) => \migrate --check risky.sql
migration lint:
  [warning] CREATE INDEX — CREATE INDEX without CONCURRENTLY
     fix: locks the table against writes while it builds; use CREATE INDEX CONCURRENTLY (in a separate, non-wrapped file)
  [warning] ALTER TABLE — ADD COLUMN NOT NULL without DEFAULT
     fix: fails on a non-empty table; add a DEFAULT (constant default is metadata-only on PostgreSQL 11+) or backfill then SET NOT NULL
```

Для многошардовой раскатки: `--canary` (сначала один шард), `--batch N` (батчами с барьером),
`--resume` (донакатить только не-применённые шарды по локальному журналу).

### Сохранённые запросы

```
shop/sharded(0) => select item_id, price from items where status = 'paid' order by item_id limit 3;
shop/sharded(0) => \save paid3
saved "paid3"
shop/sharded(0) => \run paid3
┌─────────┬───────┐
│ item_id │ price │
├─────────┼───────┤
│ 1       │ 1.5   │
│ 5       │ 7.5   │
│ 9       │ 13.5  │
└─────────┴───────┘
```

### Неинтерактивно в CI

`terox query` отдаёт результат в нужном формате; `--format envelope` несёт машиночитаемые
метаданные (сводка по шардам, ошибки, схема). Коды возврата: `0` — все шарды ответили,
`2` — частичный успех, `1` — полный провал/ошибка конфига.

```sh
$ terox query -t shop/sharded/all --mode aggregate "select count(*) from items"
┌───────┐
│ count │
├───────┤
│ 300   │
└───────┘

$ terox query -t shop/sharded/all --format csv "select item_id, status from items where item_id in (1,200) order by item_id"
shard,item_id,status
shard_0,1,paid
shard_1,200,new

$ terox query -t shop/sharded/all --format envelope "select count(*) n from items"
{
  "schema_version": 1,
  "target": "shop/sharded/all",
  "columns": ["shard", "n"],
  "rows": [["shard_0", 150], ["shard_1", 150]],
  "row_count": 2,
  "shards": { "total": 2, "ok": 2, "failed": 0, "truncated": 0 },
  "errors": [],
  "truncated": false
}

$ terox migrate -t shop/sharded/all --canary add_note.sql   # offline-превью миграции, без БД
```
