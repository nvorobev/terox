package db

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"terox/internal/cluster"
	"terox/internal/migration"
)

// liveShard собирает шард, указывающий на локальный тестовый PostgreSQL.
// Живые тесты выполняются только при заданном TEROX_LIVE, иначе пропускаются.
func liveShard(db string) cluster.Shard {
	host := os.Getenv("TEROX_LIVE_HOST")
	if host == "" {
		host = "localhost"
	}
	return cluster.Shard{
		Host:     host,
		Port:     55432,
		DB:       db,
		User:     "postgres",
		Password: "secret",
		SSLMode:  "disable",
	}
}

func liveSkip(t *testing.T) {
	if os.Getenv("TEROX_LIVE") == "" {
		t.Skip("set TEROX_LIVE=1 to run live PostgreSQL integration tests")
	}
}

// TestLiveReadOnlyBlocksWrite проверяет, что запись в режиме read-only
// отклоняется на уровне базы данных в обход эвристики.
func TestLiveReadOnlyBlocksWrite(t *testing.T) {
	liveSkip(t)
	m := NewManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := liveShard("shard_0")
	if _, err := m.Exec(ctx, s, "update items.users set status='x'", true); err == nil {
		t.Fatal("expected read-only transaction to reject the UPDATE")
	}
}

// TestLiveMigrationRoleDoesNotLeak проверяет, что после коммита миграции
// с SET LOCAL ROLE следующий запрос в том же пуле идёт от исходного пользователя.
func TestLiveMigrationRoleDoesNotLeak(t *testing.T) {
	liveSkip(t)
	m := NewManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := liveShard("shard_0")

	payload, err := migration.BuildTransactional(
		"update items.users set status = status;", "_fa", "5s", "500ms")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ExecOnce(ctx, s, payload); err != nil {
		t.Fatalf("migration exec failed: %v", err)
	}

	// Прогоняем оба соединения пула: ни одно не должно нести повышенную роль.
	for i := 0; i < 4; i++ {
		res, err := m.Exec(ctx, s, "select current_user", true)
		if err != nil {
			t.Fatalf("current_user query failed: %v", err)
		}
		if got := res.Rows[0][0]; got != "postgres" {
			t.Fatalf("role leaked: current_user = %v (want postgres)", got)
		}
	}
}

// TestLiveFanoutRead проверяет чтение по нескольким шардам и порядок результатов
// на трёх заполненных базах-шардах.
func TestLiveFanoutRead(t *testing.T) {
	liveSkip(t)
	m := NewManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	shards := []cluster.Shard{liveShard("shard_0"), liveShard("shard_1"), liveShard("shard_2")}
	for i := range shards {
		shards[i].Position = i
		shards[i].Label = shards[i].DB
	}
	results := m.Fanout(ctx, shards, "select count(*) from items.users", true, 4, 5*time.Second)
	if len(results) != 3 {
		t.Fatalf("expected 3 shard results, got %d", len(results))
	}
	for _, sr := range results {
		if sr.Err != nil {
			t.Errorf("%s: %v", sr.Shard.Label, sr.Err)
		}
	}
}

// TestLiveCopyToErrorReturnsZero проверяет (FIX A): при сбое COPY ... TO
// (синтаксически верный COPY несуществующей таблицы) CopyTo возвращает (0, err),
// а не RowsAffected недостоверного тега.
func TestLiveCopyToErrorReturnsZero(t *testing.T) {
	liveSkip(t)
	m := NewManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := liveShard("shard_0")
	n, err := m.CopyTo(ctx, s, io.Discard, "COPY (SELECT * FROM no_such_table_xyz) TO STDOUT")
	if err == nil {
		t.Fatal("expected COPY of a missing table to fail")
	}
	if n != 0 {
		t.Errorf("CopyTo must return 0 rows on error, got %d", n)
	}
}

// TestLiveCopyToReadOnly проверяет (FIX B): CopyTo выполняется в server-enforced
// READ ONLY транзакции (через BeginTx с pgx.ReadOnly), поэтому COPY с волатильной
// записью отклоняется на уровне сервера, а успешный read-only COPY возвращает строки.
func TestLiveCopyToReadOnly(t *testing.T) {
	liveSkip(t)
	m := NewManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := liveShard("shard_0")

	// Read-only COPY должен проходить и отдавать строки.
	n, err := m.CopyTo(ctx, s, io.Discard, "COPY (SELECT 1) TO STDOUT")
	if err != nil {
		t.Fatalf("read-only COPY should succeed: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row copied, got %d", n)
	}

	// Запись в подзапросе COPY должна отклоняться READ ONLY транзакцией (граница на
	// сервере): создаём временную таблицу — DDL под read-only невозможен.
	if _, err := m.CopyTo(ctx, s, io.Discard,
		"COPY (SELECT * FROM (CREATE TABLE _terox_ro_probe(x int) RETURNING 1) q) TO STDOUT"); err == nil {
		t.Error("expected READ ONLY transaction to reject a writing COPY")
	}
}
