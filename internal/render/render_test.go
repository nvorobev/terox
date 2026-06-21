package render

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"terox/internal/cluster"
	"terox/internal/db"
)

func TestErrDetailSQLState(t *testing.T) {
	pg := &pgconn.PgError{Code: "42501", Message: "permission denied for table t", Severity: "ERROR"}
	got := errDetail(pg)
	if got != "[42501] permission denied for table t" {
		t.Errorf("errDetail = %q, want clean [42501] message", got)
	}
	if strings.Count(got, "42501") != 1 {
		t.Errorf("SQLSTATE duplicated in %q", got)
	}
	// Не-pg ошибка: обычный текст, без скобок.
	if got := errDetail(errors.New("connection refused")); got != "connection refused" {
		t.Errorf("plain error = %q, want unchanged", got)
	}
}

func TestMultiMergesDifferentSchemas(t *testing.T) {
	results := []db.ShardResult{
		{
			Shard: cluster.Shard{Position: 0, Label: "rs001"},
			Result: &db.Result{
				Columns:  []string{"id", "name"},
				Rows:     [][]any{{1, "a"}},
				IsSelect: true,
			},
		},
		{
			Shard: cluster.Shard{Position: 1, Label: "rs002"},
			Result: &db.Result{
				Columns:  []string{"id", "email"}, // другая схема
				Rows:     [][]any{{2, "b@x"}},
				IsSelect: true,
			},
		},
		{
			Shard: cluster.Shard{Position: 2, Label: "rs003"},
			Err:   errContext("connection refused"),
		},
	}

	var sb strings.Builder
	Multi(&sb, results, 100)
	out := sb.String()

	for _, want := range []string{"shard", "id", "name", "email", "rs001", "rs002", "NULL", "rs003", "failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestMergeKeepsDuplicateColumnNames(t *testing.T) {
	// Один SELECT с двумя одноимёнными колонками (SELECT id, id)
	// сохраняет ОБЕ колонки при слиянии, не схлопывая их в одну.
	results := []db.ShardResult{
		{
			Shard:  cluster.Shard{Position: 0, Label: "rs001"},
			Result: &db.Result{Columns: []string{"id", "id"}, Rows: [][]any{{1, 2}}, IsSelect: true},
		},
		{
			Shard:  cluster.Shard{Position: 1, Label: "rs002"},
			Result: &db.Result{Columns: []string{"id", "id"}, Rows: [][]any{{3, 4}}, IsSelect: true},
		},
	}
	cols, rows := Merge(results)
	// shard + две колонки "id" (максимальная кратность по шардам — 2, не 1).
	if len(cols) != 3 || cols[1] != "id" || cols[2] != "id" {
		t.Fatalf("expected [shard id id], got %v", cols)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	// Оба значения первого шарда сохраняются, не перезаписываются.
	if rows[0][1] != 1 || rows[0][2] != 2 {
		t.Errorf("duplicate-column values collapsed: %v", rows[0])
	}
	if rows[1][1] != 3 || rows[1][2] != 4 {
		t.Errorf("duplicate-column values collapsed: %v", rows[1])
	}
}

func TestMultiSelectShardCountExcludesNonSelect(t *testing.T) {
	// Один шард вернул выборку, другой выполнил не-SELECT (без строк).
	// Подвал "rows across N shards" считает только SELECT-шард.
	results := []db.ShardResult{
		{
			Shard:  cluster.Shard{Position: 0, Label: "rs001"},
			Result: &db.Result{Columns: []string{"id"}, Rows: [][]any{{1}}, IsSelect: true},
		},
		{
			Shard:  cluster.Shard{Position: 1, Label: "rs002"},
			Result: &db.Result{IsSelect: false, RowsAffected: 0},
		},
	}
	var sb strings.Builder
	Multi(&sb, results, 100)
	out := sb.String()
	if !strings.Contains(out, "1 rows across 1 shards") {
		t.Errorf("footer must count only the SELECT shard (want '1 rows across 1 shards'):\n%s", out)
	}
}

func TestTrimNumeric(t *testing.T) {
	cases := map[string]string{
		"28.500000":        "28.5",
		"40.000000":        "40",
		"982355920.000000": "982355920",
		"19.90":            "19.9",
		"-0.500":           "-0.5",
		"100":              "100",    // без точки, без изменений
		"abc":              "abc",    // не число
		"1.5e10":           "1.5e10", // научная запись, без изменений
	}
	for in, want := range cases {
		if got := trimNumeric(in); got != want {
			t.Errorf("trimNumeric(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatValueUUID(t *testing.T) {
	// 16-байтовый массив, как pgx отдаёт тип uuid, должен печататься
	// канонической строкой, а не "[114 158 ...]".
	b := [16]byte{114, 158, 138, 62, 185, 202, 65, 130, 157, 158, 32, 210, 180, 48, 79, 180}
	want := "729e8a3e-b9ca-4182-9d9e-20d2b4304fb4"
	if got := formatValue(b); got != want {
		t.Errorf("formatValue(uuid) = %q, want %q", got, want)
	}
}

func TestSingleCommandTag(t *testing.T) {
	var sb strings.Builder
	Single(&sb, &db.Result{IsSelect: false, RowsAffected: 3}, 100, true)
	if !strings.Contains(sb.String(), "3 rows affected") {
		t.Errorf("missing affected count: %s", sb.String())
	}
}

func TestSingleTimingToggle(t *testing.T) {
	res := &db.Result{IsSelect: true, Columns: []string{"x"}, Rows: [][]any{{int64(1)}}}
	var on, off strings.Builder
	Single(&on, res, 100, true)
	Single(&off, res, 100, false)
	if !strings.Contains(on.String(), "1 rows,") {
		t.Errorf("timing on should show duration: %q", on.String())
	}
	if strings.Contains(off.String(), ",") || !strings.Contains(off.String(), "(1 rows)") {
		t.Errorf("timing off should hide duration: %q", off.String())
	}
}

// TestSingleTruncatedNote проверяет, что обрезанный (Truncated) результат
// сообщает о наличии ещё строк и указывает на \export, а не печатает точный итог.
func TestSingleTruncatedNote(t *testing.T) {
	res := &db.Result{
		IsSelect: true, Columns: []string{"x"},
		Rows:      [][]any{{int64(1)}, {int64(2)}},
		Truncated: true,
	}
	var sb strings.Builder
	Single(&sb, res, 2, false)
	out := sb.String()
	if !strings.Contains(out, "more rows exist") || !strings.Contains(out, "\\export") {
		t.Errorf("truncated result should note more rows exist and mention \\export; got: %q", out)
	}
}

// TestVerticalTruncatedCountIsShown проверяет, что в Truncated-ветке Vertical
// печатает фактически показанное число строк (shown), а не len(Rows): цикл мог
// остановиться раньше на локальном maxRows. Раньше счётчик врал, печатая полный
// размер материализованного среза.
func TestVerticalTruncatedCountIsShown(t *testing.T) {
	res := &db.Result{
		IsSelect: true, Columns: []string{"x"},
		Rows:      [][]any{{int64(1)}, {int64(2)}, {int64(3)}},
		Truncated: true,
	}
	var sb strings.Builder
	Vertical(&sb, res, 2, false) // maxRows=2 < len(Rows)=3 → shown==2
	out := sb.String()
	if !strings.Contains(out, "showing first 2;") {
		t.Errorf("Vertical truncated note should report shown=2, not len(Rows)=3; got: %q", out)
	}
	if strings.Contains(out, "showing first 3;") {
		t.Errorf("Vertical must not print len(Rows) in truncated note; got: %q", out)
	}
	if !strings.Contains(out, "more rows exist") || !strings.Contains(out, "\\export") {
		t.Errorf("truncated note should mention more rows and \\export; got: %q", out)
	}
}

// TestRenderNilResult проверяет защитный guard: Single и Vertical не паникуют на
// res == nil и ничего не печатают.
func TestRenderNilResult(t *testing.T) {
	var single, vertical strings.Builder
	Single(&single, nil, 100, true)
	Vertical(&vertical, nil, 100, true)
	if single.Len() != 0 {
		t.Errorf("Single(nil) should print nothing; got: %q", single.String())
	}
	if vertical.Len() != 0 {
		t.Errorf("Vertical(nil) should print nothing; got: %q", vertical.String())
	}
}

// TestMultiKeepsDuplicateColumns доказывает, что табличный многошардовый вывод
// (Multi) сохраняет повторяющиеся имена колонок (SELECT id, id), как и Merge:
// раньше Multi сворачивал их через map[string]bool и терял первое значение.
func TestMultiKeepsDuplicateColumns(t *testing.T) {
	results := []db.ShardResult{
		{
			Shard:  cluster.Shard{Position: 0, Label: "rs001"},
			Result: &db.Result{Columns: []string{"id", "id"}, Rows: [][]any{{int64(1), int64(2)}}, IsSelect: true},
		},
	}
	var sb strings.Builder
	Multi(&sb, results, 100)
	out := sb.String()
	// Оба значения дублирующейся колонки видны в выводе.
	if !strings.Contains(out, "1") || !strings.Contains(out, "2") {
		t.Errorf("Multi dropped a duplicate-column value:\n%s", out)
	}
	// В заголовке имя "id" встречается дважды (одна колонка на вхождение).
	header := strings.SplitN(out, "\n", 4)
	headerText := strings.Join(header, "\n")
	if n := strings.Count(headerText, "id"); n < 2 {
		t.Errorf("expected duplicate 'id' header columns, saw %d in:\n%s", n, out)
	}
}

// TestProvenanceColumnRenamedOnCollision доказывает, что когда пользовательский
// результат сам содержит колонку "shard", искусственная provenance-колонка
// переименовывается в "_terox_shard", чтобы заголовок не был неоднозначным.
func TestProvenanceColumnRenamedOnCollision(t *testing.T) {
	results := []db.ShardResult{
		{
			Shard:  cluster.Shard{Position: 0, Label: "rs001"},
			Result: &db.Result{Columns: []string{"shard"}, Rows: [][]any{{int64(1)}}, IsSelect: true},
		},
	}
	cols, _ := Merge(results)
	if len(cols) != 2 || cols[0] != "_terox_shard" || cols[1] != "shard" {
		t.Fatalf("expected [_terox_shard shard], got %v", cols)
	}
	var sb strings.Builder
	Multi(&sb, results, 100)
	if out := sb.String(); !strings.Contains(out, "_terox_shard") {
		t.Errorf("Multi must rename provenance column on collision:\n%s", out)
	}
}

// TestMultiDifferentMultiplicities проверяет, что при разной кратности одного
// имени по шардам в объединении берётся максимальная кратность, а у шарда с
// меньшим числом колонок недостающие слоты заполняются NULL.
func TestMultiDifferentMultiplicities(t *testing.T) {
	results := []db.ShardResult{
		{
			Shard:  cluster.Shard{Position: 0, Label: "rs001"},
			Result: &db.Result{Columns: []string{"id"}, Rows: [][]any{{int64(1)}}, IsSelect: true},
		},
		{
			Shard:  cluster.Shard{Position: 1, Label: "rs002"},
			Result: &db.Result{Columns: []string{"id", "id"}, Rows: [][]any{{int64(2), int64(3)}}, IsSelect: true},
		},
	}
	cols, rows := Merge(results)
	if len(cols) != 3 { // shard + id + id (max кратность 2)
		t.Fatalf("expected 3 columns, got %v", cols)
	}
	// Первый шард имеет одну колонку id → второй слот id у него NULL (nil).
	if rows[0][1] != int64(1) || rows[0][2] != nil {
		t.Errorf("rs001 row should be [rs001 1 nil], got %v", rows[0])
	}
	if rows[1][1] != int64(2) || rows[1][2] != int64(3) {
		t.Errorf("rs002 row should be [rs002 2 3], got %v", rows[1])
	}
}

func TestDetectTypeDrift(t *testing.T) {
	// id: int4 (oid 23) на одном шарде, int8 (oid 20) на другом -> дрейф.
	drift := []db.ShardResult{
		{Shard: cluster.Shard{Label: "rs001"}, Result: &db.Result{Columns: []string{"id", "name"}, ColTypes: []uint32{23, 25}, Rows: [][]any{{int64(1), "a"}}, IsSelect: true}},
		{Shard: cluster.Shard{Label: "rs002"}, Result: &db.Result{Columns: []string{"id", "name"}, ColTypes: []uint32{20, 25}, Rows: [][]any{{int64(2), "b"}}, IsSelect: true}},
	}
	warns := DetectTypeDrift(drift)
	if len(warns) != 1 {
		t.Fatalf("expected 1 drift warning, got %v", warns)
	}
	// Имена типов вместо сырых OID (Feature 3): int8 (oid 20) и int4 (oid 23).
	if !strings.Contains(warns[0], `"id"`) || !strings.Contains(warns[0], "int8") || !strings.Contains(warns[0], "int4") {
		t.Errorf("drift warning should name id and human type names: %q", warns[0])
	}

	// Совпадающие типы -> нет дрейфа.
	same := []db.ShardResult{
		{Result: &db.Result{Columns: []string{"id"}, ColTypes: []uint32{23}, IsSelect: true}},
		{Result: &db.Result{Columns: []string{"id"}, ColTypes: []uint32{23}, IsSelect: true}},
	}
	if w := DetectTypeDrift(same); len(w) != 0 {
		t.Errorf("matching types should not drift, got %v", w)
	}

	// Без снятых типов (ColTypes пуст) -> пропуск, без паники.
	noTypes := []db.ShardResult{{Result: &db.Result{Columns: []string{"id"}, IsSelect: true}}}
	if w := DetectTypeDrift(noTypes); len(w) != 0 {
		t.Errorf("missing ColTypes should be skipped, got %v", w)
	}
}

// TestDetectTypeDriftDuplicateNamesSameShard: повторяющиеся одноимённые колонки
// ОДНОГО шарда (SELECT a AS id, b AS id) — это не межшардовый дрейф.
func TestDetectTypeDriftDuplicateNamesSameShard(t *testing.T) {
	one := []db.ShardResult{
		{Shard: cluster.Shard{Label: "rs001"}, Result: &db.Result{
			Columns: []string{"id", "id"}, ColTypes: []uint32{23, 25}, Rows: [][]any{{int64(1), "x"}}, IsSelect: true}},
	}
	if w := DetectTypeDrift(one); len(w) != 0 {
		t.Errorf("duplicate names in ONE shard must not be reported as drift, got %v", w)
	}
}

// TestDetectTypeDriftTypeModifier: одинаковый базовый OID, но разный typmod
// (varchar(10) vs varchar(255)) тоже считается дрейфом.
func TestDetectTypeDriftTypeModifier(t *testing.T) {
	results := []db.ShardResult{
		{Shard: cluster.Shard{Label: "rs001"}, Result: &db.Result{Columns: []string{"name"}, ColTypes: []uint32{1043}, ColMods: []int32{14}, Rows: [][]any{{"a"}}, IsSelect: true}},
		{Shard: cluster.Shard{Label: "rs002"}, Result: &db.Result{Columns: []string{"name"}, ColTypes: []uint32{1043}, ColMods: []int32{259}, Rows: [][]any{{"b"}}, IsSelect: true}},
	}
	warns := DetectTypeDrift(results)
	// varchar(10) и varchar(255): typmod 14 → len 10, 259 → len 255 (Feature 3).
	if len(warns) != 1 || !strings.Contains(warns[0], "varchar(10)") || !strings.Contains(warns[0], "varchar(255)") {
		t.Errorf("typmod drift should be flagged with human varchar(n), got %v", warns)
	}
}

type errContext string

func (e errContext) Error() string { return string(e) }
