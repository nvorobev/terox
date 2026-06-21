package db

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeRows — минимальная pgx.Rows в памяти для тестов scanRows без БД.
type fakeRows struct {
	cols []string
	data [][]any
	pos  int // индекс текущей строки (с 1) после Next()
}

func (f *fakeRows) Close()                        {}
func (f *fakeRows) Err() error                    { return nil }
func (f *fakeRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (f *fakeRows) Scan(dest ...any) error        { return nil }
func (f *fakeRows) RawValues() [][]byte           { return nil }
func (f *fakeRows) Conn() *pgx.Conn               { return nil }
func (f *fakeRows) Values() ([]any, error)        { return f.data[f.pos-1], nil }

func (f *fakeRows) FieldDescriptions() []pgconn.FieldDescription {
	fd := make([]pgconn.FieldDescription, len(f.cols))
	for i, c := range f.cols {
		fd[i].Name = c
	}
	return fd
}

func (f *fakeRows) Next() bool {
	if f.pos < len(f.data) {
		f.pos++
		return true
	}
	return false
}

type fakeQuerier struct{ rows *fakeRows }

func (q fakeQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return q.rows, nil
}

// TestScanRowsCap проверяет: положительный лимит ограничивает число строк и ставит
// Truncated, когда на сервере строк больше; лимит 0 (или >= общего числа) оставляет
// все строки и не ставит Truncated.
func TestScanRowsCap(t *testing.T) {
	mk := func() fakeQuerier {
		return fakeQuerier{&fakeRows{cols: []string{"id"}, data: [][]any{{1}, {2}, {3}, {4}, {5}}}}
	}
	cases := []struct {
		limit     int
		wantRows  int
		wantTrunc bool
	}{
		{0, 5, false},  // без лимита
		{3, 3, true},   // меньше общего числа
		{5, 5, false},  // ровно общее число — без усечения
		{10, 5, false}, // больше общего числа
	}
	for _, c := range cases {
		res, err := scanRows(context.Background(), mk(), "select id from t", c.limit)
		if err != nil {
			t.Fatalf("limit %d: %v", c.limit, err)
		}
		if len(res.Rows) != c.wantRows {
			t.Errorf("limit %d: got %d rows, want %d", c.limit, len(res.Rows), c.wantRows)
		}
		if res.Truncated != c.wantTrunc {
			t.Errorf("limit %d: Truncated = %v, want %v", c.limit, res.Truncated, c.wantTrunc)
		}
	}
}

// errRows — pgx.Rows в памяти с настраиваемой ошибкой Err(), чтобы проверить, что
// scanRows проверяет rows.Err() даже на пути усечения (FIX C).
type errRows struct {
	cols []string
	data [][]any
	pos  int
	err  error // что вернёт Err() после перебора
}

func (f *errRows) Close()                        {}
func (f *errRows) Err() error                    { return f.err }
func (f *errRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (f *errRows) Scan(dest ...any) error        { return nil }
func (f *errRows) RawValues() [][]byte           { return nil }
func (f *errRows) Conn() *pgx.Conn               { return nil }
func (f *errRows) Values() ([]any, error)        { return f.data[f.pos-1], nil }

func (f *errRows) FieldDescriptions() []pgconn.FieldDescription {
	fd := make([]pgconn.FieldDescription, len(f.cols))
	for i, c := range f.cols {
		fd[i].Name = c
	}
	return fd
}

func (f *errRows) Next() bool {
	if f.pos < len(f.data) {
		f.pos++
		return true
	}
	return false
}

type errQuerier struct{ rows *errRows }

func (q errQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return q.rows, nil
}

// TestScanRowsErrOnTruncate проверяет (FIX C): при усечении настоящая ошибка
// стрима из rows.Err() пробрасывается и НЕ маскируется усечением, а отмена
// (context.Canceled) — наша же остановка — игнорируется, оставляя Truncated.
func TestScanRowsErrOnTruncate(t *testing.T) {
	mkData := func() [][]any { return [][]any{{1}, {2}, {3}, {4}, {5}} }

	// Реальная ошибка стрима при усечении не должна теряться.
	streamErr := errors.New("connection reset by peer")
	q := errQuerier{&errRows{cols: []string{"id"}, data: mkData(), err: streamErr}}
	if _, err := scanRows(context.Background(), q, "select id from t", 3); !errors.Is(err, streamErr) {
		t.Fatalf("truncated path must surface stream error, got %v", err)
	}

	// context.Canceled на пути усечения — наша остановка, не сбой: ошибки нет,
	// строки усечены, Truncated сохранён.
	q = errQuerier{&errRows{cols: []string{"id"}, data: mkData(), err: context.Canceled}}
	res, err := scanRows(context.Background(), q, "select id from t", 3)
	if err != nil {
		t.Fatalf("context.Canceled on truncate must be ignored, got %v", err)
	}
	if !res.Truncated {
		t.Error("Truncated must remain set when truncating")
	}
	if len(res.Rows) != 3 {
		t.Errorf("got %d rows, want 3", len(res.Rows))
	}

	// Полный перебор тоже пробрасывает реальную ошибку Err() (limit 0).
	q = errQuerier{&errRows{cols: []string{"id"}, data: mkData(), err: streamErr}}
	if _, err := scanRows(context.Background(), q, "select id from t", 0); !errors.Is(err, streamErr) {
		t.Fatalf("full-scan path must surface stream error, got %v", err)
	}
}
