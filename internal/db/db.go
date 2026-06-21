// Package db управляет ленивыми пулами соединений по шардам и выполняет запросы
// на одном шарде или с веером по всем шардам хранилища.
package db

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"terox/internal/cluster"
)

// Result хранит итог одного запроса на одном шарде.
type Result struct {
	Columns []string
	// ColTypes — OID типа каждой колонки (параллельно Columns), снятый из
	// FieldDescriptions. Нужен для детекта дрейфа типов одноимённой колонки между
	// шардами (Feature 3). Может быть пустым (не-SELECT / стриминговый путь).
	ColTypes []uint32
	// ColMods — type modifier каждой колонки (параллельно Columns; -1 = нет
	// модификатора), чтобы детект дрейфа различал varchar(10)/varchar(255),
	// numeric(10,2)/numeric(20,4) при одинаковом базовом OID.
	ColMods []int32
	// Cols — полная типизированная схема результата (Feature 3): имя, OID, typmod,
	// человекочитаемое имя типа, wire-формат, исходная таблица/атрибут, номер
	// вхождения и флаг synthetic. Параллельные слайсы Columns/ColTypes/ColMods
	// сохранены для совместимости и выводятся из Cols. Пуст на не-SELECT пути.
	Cols         []Column
	Rows         [][]any
	RowsAffected int64
	// ServerVersion и BackendPID — пер-шардовая provenance (Feature 13), снятые из
	// соединения БЕЗ дополнительного round-trip (ParameterStatus / PgConn().PID()).
	// 0, если путь выполнения их не снял.
	ServerVersion string
	BackendPID    uint32
	// IsSelect означает, что запрос вернул строки (а не только command tag).
	IsSelect bool
	// Truncated равно true, когда лимит строк остановил материализацию до
	// исчерпания результата (на сервере есть ещё строки). Только при чтении с лимитом.
	Truncated bool
	Duration  time.Duration
}

// ShardResult связывает Result (или ошибку) с шардом, откуда он получен.
type ShardResult struct {
	Shard  cluster.Shard
	Result *Result
	Err    error
}

// Manager владеет пулами соединений pgx с ключом host+db и переиспользует их
// между запросами.
type Manager struct {
	mu    sync.Mutex
	pools map[string]*pgxpool.Pool

	// readStmtTimeout, если задан, применяется как SET LOCAL statement_timeout в
	// каждой read-only транзакции, ограничивая чтения. Строка длительности
	// PostgreSQL ("300ms", "5s").
	readStmtTimeout string
}

// SetReadTimeout задаёт statement_timeout для последующих read-запросов.
// Под мьютексом, чтобы фоновая загрузка каталога могла читать параллельно с
// основным циклом без гонки за это поле.
func (m *Manager) SetReadTimeout(t string) {
	m.mu.Lock()
	m.readStmtTimeout = t
	m.mu.Unlock()
}

// readTimeout возвращает текущий read statement_timeout под мьютексом.
func (m *Manager) readTimeout() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readStmtTimeout
}

// NewManager создаёт пустой менеджер пулов.
func NewManager() *Manager {
	return &Manager{pools: map[string]*pgxpool.Pool{}}
}

func poolKey(s cluster.Shard) string {
	// Нормализуем sslmode так же, как dsn() ("" -> "disable"), чтобы Shard с пустым
	// режимом давал тот же ключ пула, что и явный "disable".
	ssl := s.SSLMode
	if ssl == "" {
		ssl = "disable"
	}
	// Пароль И все параметры профиля подключения входят в sha256 c NUL-разделителями:
	// смена пароля/TLS-режима/sslrootcert/клиентского сертификата/connect_timeout
	// инвалидирует пул, а не переиспользует соединение со старым профилем. NUL-разделение
	// исключает коллизию, когда значение содержит подстроку-разделитель; секрет в ключ
	// открытым не попадает.
	var b strings.Builder
	for _, part := range []string{s.Password, ssl, s.SSLRootCert, s.SSLCert, s.SSLKey, s.ConnectTimeout.String(), s.PassFile} {
		b.WriteString(part)
		b.WriteByte(0)
	}
	h := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%s:%d/%s@%s?sslmode=%s#%x", s.Host, s.Port, s.DB, s.User, ssl, h[:16])
}

// dsn строит URL postgres:// с экранированием каждого компонента, чтобы
// пароли/пользователи/базы с пробелами, кавычками и спецсимволами подключались
// корректно.
func dsn(s cluster.Shard) string {
	sslmode := s.SSLMode
	if sslmode == "" {
		sslmode = "disable"
	}
	u := url.URL{
		Scheme: "postgresql",
		User:   url.UserPassword(s.User, s.Password),
		Host:   net.JoinHostPort(s.Host, strconv.Itoa(s.Port)),
		Path:   "/" + s.DB,
	}
	q := url.Values{}
	q.Set("sslmode", sslmode)
	if s.SSLRootCert != "" {
		q.Set("sslrootcert", s.SSLRootCert)
	}
	if s.SSLCert != "" {
		q.Set("sslcert", s.SSLCert)
	}
	if s.SSLKey != "" {
		q.Set("sslkey", s.SSLKey)
	}
	if s.ConnectTimeout > 0 {
		// libpq connect_timeout — целые секунды; субсекундное округляем вверх до 1.
		secs := int(s.ConnectTimeout / time.Second)
		if secs < 1 {
			secs = 1
		}
		q.Set("connect_timeout", strconv.Itoa(secs))
	}
	if s.PassFile != "" {
		q.Set("passfile", s.PassFile)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// RedactDSN маскирует пароль в postgres-URL, чтобы DSN можно было безопасно
// показать в ошибке/логе (секрет не утекает). Возвращает строку как есть, если
// это не разбираемый URL с паролем.
func RedactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return dsn
	}
	if _, hasPw := u.User.Password(); hasPw {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return u.String()
}

// pool возвращает закешированный пул для шарда, создавая его лениво.
func (m *Manager) pool(ctx context.Context, s cluster.Shard) (*pgxpool.Pool, error) {
	key := poolKey(s)
	m.mu.Lock()
	if p, ok := m.pools[key]; ok {
		m.mu.Unlock()
		return p, nil
	}
	m.mu.Unlock()

	cfg, err := pgxpool.ParseConfig(dsn(s))
	if err != nil {
		// Ошибка ParseConfig может включать сам DSN с открытым паролем — редактируем,
		// чтобы секрет не утёк в сообщение/лог.
		return nil, fmt.Errorf("invalid connection string for %s: %w", RedactDSN(dsn(s)), err)
	}
	cfg.MinConns = 0
	cfg.MaxConns = 2
	cfg.MaxConnIdleTime = 5 * time.Minute
	// Ограничиваем общий срок жизни соединения, чтобы давно живущие backend
	// периодически пересоздавались (освобождение серверной памяти, подхват
	// изменений ролей/маршрутов pgbouncer), а не висели бесконечно.
	cfg.MaxConnLifetime = 30 * time.Minute
	// С PostgreSQL общаемся через pgbouncer в режиме transaction pooling.
	// Простой протокол запросов не создаёт серверных именованных prepared-
	// statement (несовместимых с transaction pooling) и позволяет одному Exec
	// нести многооператорную/многотранзакционную миграцию одним round-trip на
	// одном backend-соединении.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	// Простой протокол не использует кеши prepared-statement / description;
	// отключаем их, чтобы соединение не выделяло неиспользуемые LRU-карты.
	cfg.ConnConfig.StatementCacheCapacity = 0
	cfg.ConnConfig.DescriptionCacheCapacity = 0
	// Помечаем соединение, чтобы каждая операция terox была видна в
	// pg_stat_activity / логах сервера (и pgbouncer), а не как анонимный psql.
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "terox"

	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	// Другая горутина могла создать его параллельно.
	if existing, ok := m.pools[key]; ok {
		m.mu.Unlock()
		p.Close()
		return existing, nil
	}
	m.pools[key] = p
	m.mu.Unlock()
	return p, nil
}

// Close освобождает все пулы.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.pools {
		p.Close()
	}
	m.pools = map[string]*pgxpool.Pool{}
}

// Exec выполняет sql на одном шарде. При readOnly запрос идёт в транзакции
// READ ONLY с откатом, что блокирует любую запись на уровне БД. Результат
// материализуется целиком; используйте ExecLimit для ограничения числа строк
// в памяти.
func (m *Manager) Exec(ctx context.Context, s cluster.Shard, sql string, readOnly bool) (*Result, error) {
	return m.ExecLimit(ctx, s, sql, readOnly, 0)
}

// ExecLimit — это Exec с ограничением числа материализуемых строк (0 = без
// лимита). Если строк больше лимита, хранятся только limit, а в Result
// выставляется Truncated, чтобы случайный широкий SELECT не исчерпал память клиента.
func (m *Manager) ExecLimit(ctx context.Context, s cluster.Shard, sql string, readOnly bool, limit int) (*Result, error) {
	p, err := m.pool(ctx, s)
	if err != nil {
		return nil, err
	}
	start := time.Now()

	if readOnly {
		res, err := m.execReadOnly(ctx, p, sql, limit)
		if res != nil {
			res.Duration = time.Since(start)
		}
		return res, err
	}

	res, err := m.execRows(ctx, p, sql, limit)
	if res != nil {
		res.Duration = time.Since(start)
	}
	return res, err
}

// execReadOnly выполняет sql в read-only транзакции, которая всегда
// откатывается. Откат идёт со свежим контекстом, чтобы при уже истёкшем или
// отменённом контексте запроса ROLLBACK всё равно завершился чисто и соединение
// вернулось в пул здоровым.
func (m *Manager) execReadOnly(ctx context.Context, p *pgxpool.Pool, sql string, limit int) (*Result, error) {
	tx, err := p.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() {
		rbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rbCtx)
	}()
	// Ограничиваем чтение сессионным statement_timeout (SET LOCAL откатывается
	// вместе с транзакцией, поэтому не утекает при pgbouncer pooling). Одинарные
	// кавычки экранируются; значение — проверенная длительность PG. Если
	// применить ограничение не удалось, прерываемся, а не читаем без лимита.
	if t := m.readTimeout(); t != "" {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+strings.ReplaceAll(t, "'", "''")+"'"); err != nil {
			return nil, fmt.Errorf("could not set statement_timeout=%s (read aborted to avoid running unbounded): %w", t, err)
		}
	}
	res, err := scanRows(ctx, tx, sql, limit)
	// Пер-шардовая provenance без отдельного round-trip: backend PID и версия сервера
	// доступны прямо из соединения транзакции (Feature 13).
	if res != nil {
		if c := tx.Conn(); c != nil {
			pc := c.PgConn()
			res.BackendPID = pc.PID()
			res.ServerVersion = pc.ParameterStatus("server_version")
		}
	}
	return res, err
}

// hintParamError превращает невнятную ошибку про $N-плейсхолдер в понятную: terox
// не биндит параметры запроса — литералы нужно подставлять прямо в SQL.
func hintParamError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "there is no parameter") ||
		strings.Contains(msg, "insufficient arguments") || strings.Contains(msg, "unused argument") {
		return fmt.Errorf("$N placeholders are not bound — terox does not bind query parameters; inline literal values into the SQL instead (underlying: %w)", err)
	}
	return err
}

// execRows выполняет sql напрямую на пуле (путь записи).
func (m *Manager) execRows(ctx context.Context, p *pgxpool.Pool, sql string, limit int) (*Result, error) {
	return scanRows(ctx, p, sql, limit)
}

// querier реализуется и *pgxpool.Pool, и pgx.Tx.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// scanRows выполняет sql и материализует результат. При limit > 0 хранит не
// более limit строк и выставляет Truncated, если на сервере было больше (одна
// лишняя строка читается для проверки и отбрасывается). limit <= 0
// материализует всё.
func scanRows(ctx context.Context, q querier, sql string, limit int) (*Result, error) {
	// Пул уже сконфигурирован простым протоколом (DefaultQueryExecMode), поэтому запрос
	// идёт одним round-trip без серверных prepared statements (pgbouncer-safe).
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, hintParamError(err)
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	cols := make([]string, len(fields))
	colTypes := make([]uint32, len(fields))
	colMods := make([]int32, len(fields))
	schema := make([]Column, len(fields))
	occ := make(map[string]int, len(fields))
	for i, f := range fields {
		name := string(f.Name)
		cols[i] = name
		colTypes[i] = f.DataTypeOID
		colMods[i] = f.TypeModifier
		schema[i] = Column{
			Name:           name,
			DataTypeOID:    f.DataTypeOID,
			TypeModifier:   f.TypeModifier,
			TypeName:       TypeName(f.DataTypeOID, f.TypeModifier),
			Format:         f.Format,
			SourceTableOID: f.TableOID,
			SourceAttr:     int16(f.TableAttributeNumber),
			Occurrence:     occ[name],
		}
		occ[name]++
	}

	var data [][]any
	truncated := false
	for rows.Next() {
		if limit > 0 && len(data) >= limit {
			// Уже есть `limit` строк, и существует ещё одна: результат больше
			// лимита. Прекращаем материализацию — рендерер показывает не более
			// `limit`, а \export при необходимости перечитывает весь набор.
			truncated = true
			break
		}
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		// Копируем в свежий слайс; pgx переиспользует буферы между итерациями.
		row := make([]any, len(vals))
		copy(row, vals)
		data = append(data, row)
	}
	// rows.Err() проверяем в обоих случаях. При полном переборе он отдаёт реальную
	// ошибку стрима. При раннем выходе по усечению (truncated) набор досрочно
	// оборван нашим break, поэтому pgx обычно проставляет context.Canceled —
	// это НЕ сбой, а наша же остановка, её игнорируем и сохраняем Truncated.
	// Любую другую ошибку Err() — настоящий сбой чтения/сети/протокола —
	// пробрасываем, чтобы усечение не маскировало реальную ошибку стрима.
	if err := rows.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return nil, err
	}

	tag := rows.CommandTag()
	return &Result{
		Columns:      cols,
		ColTypes:     colTypes,
		ColMods:      colMods,
		Cols:         schema,
		Rows:         data,
		RowsAffected: tag.RowsAffected(),
		IsSelect:     len(cols) > 0,
		Truncated:    truncated,
	}, nil
}

// StreamRead выполняет sql в read-only транзакции с обязательным откатом и
// вызывает onRow для каждой строки БЕЗ материализации всего набора — безопасный
// по памяти путь для \export большого результата. onCols вызывается один раз с
// именами столбцов перед первой строкой. Учитывает сессионный read timeout, как
// execReadOnly.
func (m *Manager) StreamRead(ctx context.Context, s cluster.Shard, sql string, onSchema func([]Column), onRow func([]any) error) error {
	p, err := m.pool(ctx, s)
	if err != nil {
		return err
	}
	tx, err := p.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() {
		rbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rbCtx)
	}()
	if t := m.readTimeout(); t != "" {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+strings.ReplaceAll(t, "'", "''")+"'"); err != nil {
			return fmt.Errorf("could not set statement_timeout=%s (read aborted to avoid running unbounded): %w", t, err)
		}
	}
	rows, err := tx.Query(ctx, sql)
	if err != nil {
		return hintParamError(err)
	}
	defer rows.Close()

	if onSchema != nil {
		fields := rows.FieldDescriptions()
		schema := make([]Column, len(fields))
		occ := make(map[string]int, len(fields))
		for i, f := range fields {
			name := string(f.Name)
			schema[i] = Column{
				Name:           name,
				DataTypeOID:    f.DataTypeOID,
				TypeModifier:   f.TypeModifier,
				TypeName:       TypeName(f.DataTypeOID, f.TypeModifier),
				Format:         f.Format,
				SourceTableOID: f.TableOID,
				SourceAttr:     int16(f.TableAttributeNumber),
				Occurrence:     occ[name],
			}
			occ[name]++
		}
		onSchema(schema)
	}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return err
		}
		row := make([]any, len(vals))
		copy(row, vals)
		if err := onRow(row); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Fanout выполняет sql параллельно по всем шардам с ограничением concurrency и
// возвращает результаты, отсортированные по позиции шарда. У каждого шарда свой
// таймаут, производный от родительского ctx.
func (m *Manager) Fanout(ctx context.Context, shards []cluster.Shard, sql string, readOnly bool, concurrency int, perShardTimeout time.Duration) []ShardResult {
	return m.FanoutProgress(ctx, shards, sql, readOnly, concurrency, perShardTimeout, 0, nil)
}

// FanoutProgress — это Fanout с лимитом строк на шард (limit, 0 = без лимита) и
// колбэком onDone, вызываемым (возможно из разных горутин) по завершении
// каждого шарда — для живого прогресса и реакции на отмену. Отмена ctx
// прерывает выполняемые запросы шардов.
func (m *Manager) FanoutProgress(ctx context.Context, shards []cluster.Shard, sql string, readOnly bool, concurrency int, perShardTimeout time.Duration, limit int, onDone func(done, total int, sr ShardResult)) []ShardResult {
	if concurrency <= 0 {
		concurrency = 16
	}
	results := make([]ShardResult, len(shards))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var completed int64

	for i, s := range shards {
		// Берём слот ДО запуска, чтобы одновременно было не более `concurrency`
		// горутин, а отменённый ctx сразу пропускал ещё не стартовавшие шарды.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			sr := ShardResult{Shard: s, Err: ctx.Err()}
			results[i] = sr
			if onDone != nil {
				onDone(int(atomic.AddInt64(&completed, 1)), len(shards), sr)
			}
			continue
		}
		wg.Add(1)
		go func(i int, s cluster.Shard) {
			defer wg.Done()
			defer func() { <-sem }()

			shardCtx := ctx
			cancel := context.CancelFunc(func() {})
			if perShardTimeout > 0 {
				shardCtx, cancel = context.WithTimeout(ctx, perShardTimeout)
			}
			defer cancel()

			res, err := m.ExecLimit(shardCtx, s, sql, readOnly, limit)
			sr := ShardResult{Shard: s, Result: res, Err: err}
			results[i] = sr
			if onDone != nil {
				onDone(int(atomic.AddInt64(&completed, 1)), len(shards), sr)
			}
		}(i, s)
	}
	wg.Wait()

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Shard.Position < results[j].Shard.Position
	})
	return results
}

// ExecScript выполняет каждый оператор SQL-скрипта на одном шарде как
// отдельный autocommit-оператор (без обёртывающей транзакции), как "psql -f".
// Нужно для операторов вроде CREATE INDEX CONCURRENTLY, которые нельзя
// выполнять в блоке транзакции. Выполнение останавливается на первом сбойном
// операторе (семантика ON_ERROR_STOP), и ошибка называет проблемный оператор.
func (m *Manager) ExecScript(ctx context.Context, s cluster.Shard, statements []string) (int64, error) {
	p, err := m.pool(ctx, s)
	if err != nil {
		return 0, err
	}
	var total int64
	for i, stmt := range statements {
		tag, err := p.Exec(ctx, stmt)
		if err != nil {
			return total, fmt.Errorf("statement %d/%d failed: %w", i+1, len(statements), err)
		}
		total += tag.RowsAffected()
	}
	return total, nil
}

// ExecOnce выполняет sql одним exec простого протокола (один round-trip),
// сохраняя многотранзакционную структуру, например:
//
//	set role _fa;
//	begin; set statement_timeout='500ms'; commit;
//	begin; update ...; commit;
//
// Один exec гарантирует, что все операторы попадут на одно backend-соединение —
// это критично при pgbouncer transaction pooling, где отдельные exec ушли бы на
// разные backend и role/timeout не применились бы к миграции. Возвращает суммарное
// число затронутых строк по всем операторам.
func (m *Manager) ExecOnce(ctx context.Context, s cluster.Shard, sql string) (int64, error) {
	p, err := m.pool(ctx, s)
	if err != nil {
		return 0, err
	}
	conn, err := p.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()

	mrr := conn.Conn().PgConn().Exec(ctx, sql)
	var affected int64
	for mrr.NextResult() {
		rr := mrr.ResultReader()
		tag, err := rr.Close()
		if err != nil {
			_ = mrr.Close()
			return 0, err // весь exec — одна транзакция: при сбое откат, 0 изменённых строк
		}
		// Строки, возвращённые завершающими SELECT, не считаем затронутыми.
		if !tag.Select() {
			affected += tag.RowsAffected()
		}
	}
	if err := mrr.Close(); err != nil {
		return 0, err
	}
	return affected, nil
}

// CopyTo выполняет COPY ... TO STDOUT на шарде в server-enforced READ ONLY
// транзакции (как и обычные чтения), поэтому даже волатильная функция в
// подзапросе COPY (SELECT …) TO не сможет писать — граница на сервере, а не на
// эвристике IsWrite. Пишет сырой поток в w (сервер видит STDOUT, не путь).
func (m *Manager) CopyTo(ctx context.Context, s cluster.Shard, w io.Writer, sql string) (int64, error) {
	p, err := m.pool(ctx, s)
	if err != nil {
		return 0, err
	}
	conn, err := p.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	// Server-enforced READ ONLY на уровне опций транзакции (как execReadOnly/
	// StreamRead), а не ручным "SET TRANSACTION READ ONLY": единая модель по пакету
	// и read-only задаётся ещё в BEGIN, а не отдельным оператором после.
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, err
	}
	defer func() {
		rbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rbCtx)
	}()
	if t := m.readTimeout(); t != "" {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+strings.ReplaceAll(t, "'", "''")+"'"); err != nil {
			return 0, err
		}
	}
	tag, err := tx.Conn().PgConn().CopyTo(ctx, w, sql)
	if err != nil {
		// COPY не прошёл — RowsAffected недостоверен; возвращаем 0 (как CopyFromTx),
		// чтобы вызывающий не засчитал ложный прогресс по сбойному стриму.
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CopyFromTx выполняет COPY ... FROM STDIN под той же защитной моделью, что и
// обычная запись/миграция (R-NEW-1): на одном backend-соединении открывается
// транзакция, применяются guard-операторы (SET LOCAL ROLE + SET LOCAL
// statement_timeout + SET LOCAL lock_timeout, переданные вызывающим как setup),
// затем выполняется COPY, и только при успехе делается COMMIT. Любая ошибка (в
// т.ч. отмена ctx) откатывает транзакцию СВЕЖИМ контекстом, поэтому частично
// загруженные строки не фиксируются, а соединение возвращается в пул здоровым,
// без «битой» транзакции. setup уже провалидирован/экранирован (migration.
// SessionGuards), поэтому здесь он выполняется как есть.
//
// Это закрывает расхождение, при котором обычные записи шли под migration role с
// локальными таймаутами, а \copy FROM — под ролью подключения и без них.
func (m *Manager) CopyFromTx(ctx context.Context, s cluster.Shard, r io.Reader, sql string, setup []string) (int64, error) {
	p, err := m.pool(ctx, s)
	if err != nil {
		return 0, err
	}
	conn, err := p.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Откат СВЕЖИМ контекстом: даже при истёкшем/отменённом ctx запроса ROLLBACK
		// завершится и соединение вернётся в пул чистым (как в execReadOnly).
		rbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rbCtx)
	}()

	for _, stmt := range setup {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return 0, fmt.Errorf("copy setup %q failed: %w", stmt, err)
		}
	}

	tag, err := tx.Conn().PgConn().CopyFrom(ctx, r, sql)
	if err != nil {
		// COPY не прошёл — транзакция откатится (defer), ничего не зафиксировано.
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		// COMMIT не прошёл — транзакция откатывается, значит зафиксировано 0 строк
		// (как в ExecOnce). Возврат tag.RowsAffected() здесь дал бы ложный прогресс:
		// вызывающий мог бы засчитать в ledger незакоммиченные строки.
		return 0, err
	}
	committed = true
	return tag.RowsAffected(), nil
}

// ExecResult — итог веера записи/миграции для одного шарда.
type ExecResult struct {
	Shard    cluster.Shard
	Affected int64
	Err      error
	Duration time.Duration
}

// ForEachShard выполняет fn на каждом шарде параллельно (с ограничением
// concurrency) и возвращает результаты в порядке позиции. При stopOnError=false
// сбой на одном шарде не прерывает остальные (режим сбора статуса, напр. \ping).
// При stopOnError=true ПЕРВЫЙ сбой отменяет группу: ещё не стартовавшие шарды
// пропускаются, а выполняемые отменяются — безопасный режим для веера
// миграции/записи, чтобы сбойное изменение не уходило дальше. В любом случае
// частично применённую миграцию можно перезапустить.
func (m *Manager) ForEachShard(ctx context.Context, shards []cluster.Shard, concurrency int, perShardTimeout time.Duration, stopOnError bool, fn func(context.Context, cluster.Shard) (int64, error)) []ExecResult {
	if concurrency <= 0 {
		concurrency = 16
	}
	// При stopOnError производный отменяемый контекст позволяет первому сбою
	// прервать оставшиеся и выполняемые шарды, не трогая ctx вызывающего.
	groupCtx := ctx
	stop := context.CancelFunc(func() {})
	if stopOnError {
		groupCtx, stop = context.WithCancel(ctx)
	}
	defer stop()

	results := make([]ExecResult, len(shards))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i, s := range shards {
		// Когда группа отменена (сработал stop-on-error или вызывающий нажал
		// Ctrl-C), ДЕТЕРМИНИРОВАННО пропускаем оставшиеся шарды: обычный
		// select acquire/Done выбирал бы случайно при свободном слоте, позволяя
		// шарду стартовать после остановки. (Случаи взаимоисключающие, поэтому
		// слот в ветке Done не берётся — утечки семафора нет.)
		select {
		case <-groupCtx.Done():
			results[i] = ExecResult{Shard: s, Err: groupCtx.Err()}
			continue
		default:
		}
		// Берём слот до запуска (ограничиваем горутины; отмена ctx пропускает
		// ещё не стартовавшие шарды).
		select {
		case sem <- struct{}{}:
		case <-groupCtx.Done():
			results[i] = ExecResult{Shard: s, Err: groupCtx.Err()}
			continue
		}
		wg.Add(1)
		go func(i int, s cluster.Shard) {
			defer wg.Done()
			defer func() { <-sem }()

			shardCtx := groupCtx
			cancel := context.CancelFunc(func() {})
			if perShardTimeout > 0 {
				shardCtx, cancel = context.WithTimeout(groupCtx, perShardTimeout)
			}
			defer cancel()

			start := time.Now()
			affected, err := fn(shardCtx, s)
			results[i] = ExecResult{Shard: s, Affected: affected, Err: err, Duration: time.Since(start)}
			if stopOnError && err != nil {
				stop() // прерываем оставшиеся (и выполняемые) шарды
			}
		}(i, s)
	}
	wg.Wait()

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Shard.Position < results[j].Shard.Position
	})
	return results
}

// Ping проверяет доступность одного шарда.
func (m *Manager) Ping(ctx context.Context, s cluster.Shard) error {
	p, err := m.pool(ctx, s)
	if err != nil {
		return err
	}
	return p.Ping(ctx)
}
