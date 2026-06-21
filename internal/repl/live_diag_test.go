package repl

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"terox/internal/cluster"
	"terox/internal/config"
	"terox/internal/db"
)

// Интеграционные тесты против локальной test-БД (terox_test на 127.0.0.1:5433,
// см. config test/local). Включаются TEROX_TEST_LIVE=1. Проверяют, что SQL
// диагностических команд (Feature 9/10), написанный без живой БД, реально
// исполняется на PostgreSQL 16 без ошибок. Хост/порт можно переопределить
// TEROX_TEST_HOST/TEROX_TEST_PORT.
func liveTestShard(t *testing.T) (*db.Manager, cluster.Shard) {
	t.Helper()
	if os.Getenv("TEROX_TEST_LIVE") == "" {
		t.Skip("set TEROX_TEST_LIVE=1 to run integration tests against terox_test")
	}
	host := os.Getenv("TEROX_TEST_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	mgr := db.NewManager()
	t.Cleanup(mgr.Close)
	return mgr, cluster.Shard{
		Position: 0, Label: "local", Host: host, Port: 5433,
		DB: "terox_test", User: "postgres", Password: "test", SSLMode: "disable",
	}
}

func TestLiveDiagnosticSQL(t *testing.T) {
	mgr, shard := liveTestShard(t)
	ver := 160000
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if r, err := mgr.Exec(ctx, shard, `SELECT current_setting('server_version_num')::int`, true); err == nil && r != nil && len(r.Rows) > 0 {
		ver = int(asInt64(r.Rows[0][0]))
	}

	cases := []struct {
		name string
		sql  string
		skip bool
	}{
		{"activity", activitySQL(false), false},
		{"activity-all", activitySQL(true), false},
		{"blockers", blockersSQL(), false},
		{"locks", locksSQL(), false},
		{"longtx", longtxSQL(60), false},
	}
	for _, c := range cases {
		if c.skip {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := mgr.Exec(ctx, shard, c.sql, true); err != nil {
				t.Errorf("%s SQL failed on live PG%d:\n%s\nerr: %v", c.name, ver/10000, c.sql, err)
			}
		})
	}
}

func TestLiveStatementsSQL(t *testing.T) {
	mgr, shard := liveTestShard(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ver := 160000
	if r, err := mgr.Exec(ctx, shard, `SELECT current_setting('server_version_num')::int`, true); err == nil && r != nil && len(r.Rows) > 0 {
		ver = int(asInt64(r.Rows[0][0]))
	}
	// pg_stat_statements может быть не установлен (нужен shared_preload_libraries) —
	// это нормально; при отсутствии расширения пропускаем (продукт сам отдаёт
	// per-shard ERR, см. runDiagQuery). Пробуем установить, затем проверяем наличие
	// по числу строк (а не по ошибке: SELECT без совпадений ошибки не даёт).
	_, _ = mgr.Exec(ctx, shard, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`, true)
	// Проверяем РЕАЛЬНУЮ работоспособность запросом к самому представлению: расширение
	// может быть зарегистрировано в pg_extension, но при этом не загружено через
	// shared_preload_libraries (тогда SELECT падает с 55000). Регистрации недостаточно.
	if _, err := mgr.Exec(ctx, shard, `SELECT 1 FROM pg_stat_statements LIMIT 1`, true); err != nil {
		t.Skipf("pg_stat_statements not usable (needs shared_preload_libraries): %v", err)
	}
	for _, ob := range []string{"total", "mean", "calls", "rows"} {
		if _, err := mgr.Exec(ctx, shard, statementsSQL(ver, ob, 5), true); err != nil {
			t.Errorf("statements SQL (order %s) failed: %v", ob, err)
		}
	}
}

func TestLiveCopyRoundTrip(t *testing.T) {
	mgr, shard := liveTestShard(t)
	ctx := context.Background()
	const tbl = "terox_copy_test"
	if _, err := mgr.Exec(ctx, shard, "DROP TABLE IF EXISTS "+tbl, false); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Exec(ctx, shard, "CREATE TABLE "+tbl+" (id int, name text)", false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = mgr.Exec(context.Background(), shard, "DROP TABLE IF EXISTS "+tbl, false) })

	dir := t.TempDir()
	in := filepath.Join(dir, "in.csv")
	if err := os.WriteFile(in, []byte("id,name\n1,alice\n2,bob\n3,carol\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &REPL{out: io.Discard, cfg: &config.Config{}, mgr: mgr,
		targets: []cluster.Shard{shard}, writeMode: true, writeApprove: false}

	// COPY FROM (загрузка).
	if err := r.doCopy("\\copy " + tbl + " from " + in + " csv"); err != nil {
		t.Fatalf("copy from: %v", err)
	}
	res, err := mgr.Exec(ctx, shard, "SELECT count(*) FROM "+tbl, true)
	if err != nil || res == nil || len(res.Rows) == 0 || asInt64(res.Rows[0][0]) != 3 {
		t.Fatalf("expected 3 rows loaded, got %v (err %v)", res, err)
	}

	// COPY (query) TO (выгрузка).
	out := filepath.Join(dir, "out.csv")
	if err := r.doCopy("\\copy (select * from " + tbl + " order by id) to " + out + " csv"); err != nil {
		t.Fatalf("copy to: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"id,name", "1,alice", "2,bob", "3,carol"} {
		if !strings.Contains(got, want) {
			t.Errorf("export missing %q in:\n%s", want, got)
		}
	}

	// COPY FROM без режима записи должен отклоняться.
	ro := &REPL{out: io.Discard, cfg: &config.Config{}, mgr: mgr, targets: []cluster.Shard{shard}, writeMode: false}
	if err := ro.doCopy("\\copy " + tbl + " from " + in + " csv"); err == nil || !strings.Contains(err.Error(), "write mode") {
		t.Errorf("copy from without write mode should be refused, got %v", err)
	}
}

func TestLiveAdvise(t *testing.T) {
	mgr, shard := liveTestShard(t)
	ctx := context.Background()
	const tbl = "terox_advise_test"
	_, _ = mgr.Exec(ctx, shard, "DROP TABLE IF EXISTS "+tbl, false)
	// varchar -> в плане фильтр рендерится как ((email)::text = …): проверяем, что
	// advisor извлекает РЕАЛЬНЫЙ столбец email, а не имя типа text (HIGH-фикс).
	if _, err := mgr.Exec(ctx, shard, "CREATE TABLE "+tbl+" (id int, email varchar(100))", false); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Exec(ctx, shard, "INSERT INTO "+tbl+" SELECT g, 'u'||g||'@x' FROM generate_series(1,500) g", false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = mgr.Exec(context.Background(), shard, "DROP TABLE IF EXISTS "+tbl, false) })

	var buf bytes.Buffer
	r := &REPL{out: &buf, cfg: &config.Config{}, mgr: mgr, targets: []cluster.Shard{shard}}

	// Без индекса по email — должно предложить индекс.
	if err := r.doAdvise("\\advise select * from " + tbl + " where email = 'u1@x'"); err != nil {
		t.Fatalf("advise: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "CREATE INDEX") || !strings.Contains(out, "(email)") {
		t.Errorf("expected an index suggestion on (email), got:\n%s", out)
	}
	if strings.Contains(out, "(text)") {
		t.Errorf("must not suggest an index on the cast type name 'text':\n%s", out)
	}

	// Создаём индекс по email — повторный advise НЕ должен предлагать email.
	if _, err := mgr.Exec(ctx, shard, "CREATE INDEX ON "+tbl+" (email)", false); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := r.doAdvise("\\advise select * from " + tbl + " where email = 'u1@x'"); err != nil {
		t.Fatalf("advise (2): %v", err)
	}
	if out2 := buf.String(); strings.Contains(out2, "CREATE INDEX") {
		t.Errorf("email is now indexed — should not be suggested again, got:\n%s", out2)
	}
}
