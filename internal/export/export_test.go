package export

import (
	"database/sql/driver"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestWriteCSV(t *testing.T) {
	var b strings.Builder
	cols := []string{"shard", "id", "name"}
	rows := [][]any{
		{"shard_0", int64(1), "alpha"},
		{"shard_1", int64(2), nil},
	}
	if err := WriteCSV(&b, cols, rows); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"shard,id,name", "shard_0,1,alpha", "shard_1,2,"} {
		if !strings.Contains(out, want) {
			t.Errorf("CSV missing %q:\n%s", want, out)
		}
	}
}

// pgNumeric имитирует pgtype.Numeric: driver.Valuer, возвращающий десятичную строку.
type pgNumeric string

func (n pgNumeric) Value() (driver.Value, error) { return string(n), nil }

func TestCellTextNotTrimmed(t *testing.T) {
	// Обычный text/varchar передаётся как есть; обрезаются только числовые значения.
	if got := cell("1.0.0"); got != "1.0.0" {
		t.Errorf("text %q corrupted to %q", "1.0.0", got)
	}
	if got := cell("2.10"); got != "2.10" {
		t.Errorf("text %q corrupted to %q", "2.10", got)
	}
	if got := cell(pgNumeric("28.500000")); got != "28.5" {
		t.Errorf("numeric trim: got %q want 28.5", got)
	}
}

func TestCellFloat(t *testing.T) {
	if got := cell(float64(1234567)); got != "1234567" {
		t.Errorf("float8: got %q want 1234567", got)
	}
	if got := cell(float64(1000000)); got != "1000000" {
		t.Errorf("float8: got %q want 1000000", got)
	}
}

func TestJSONNumericUnquoted(t *testing.T) {
	var b strings.Builder
	if err := WriteJSON(&b, []string{"n"}, [][]any{{pgNumeric("28.500000")}}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, `"n": 28.5`) { // число без кавычек, обрезанное
		t.Errorf("JSON numeric not unquoted/trimmed:\n%s", out)
	}
}

func TestJSONNonFiniteFloat(t *testing.T) {
	// Ячейка NaN/Inf не прерывает экспорт, а становится строкой,
	// как в to_json у PostgreSQL.
	var b strings.Builder
	rows := [][]any{{math.NaN()}, {math.Inf(1)}, {math.Inf(-1)}, {float64(1.5)}}
	if err := WriteJSON(&b, []string{"v"}, rows); err != nil {
		t.Fatalf("export aborted by non-finite float: %v", err)
	}
	out := b.String()
	for _, want := range []string{`"v": "NaN"`, `"v": "Infinity"`, `"v": "-Infinity"`, `"v": 1.5`} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON missing %q:\n%s", want, out)
		}
	}
}

func TestBytesValueHex(t *testing.T) {
	if got := cell([]byte{0xde, 0xad, 0xbe, 0xef}); got != `\xdeadbeef` {
		t.Errorf("binary bytea: got %q want \\xdeadbeef", got)
	}
	if got := cell([]byte("hello")); got != "hello" { // валидный UTF-8 остаётся текстом
		t.Errorf("text bytes: got %q want hello", got)
	}
}

func TestUUIDExport(t *testing.T) {
	// 16-байтовый массив (тип uuid от pgx) экспортируется канонической строкой,
	// а не "[114 158 ...]" — и в CSV, и в JSON.
	u := [16]byte{114, 158, 138, 62, 185, 202, 65, 130, 157, 158, 32, 210, 180, 48, 79, 180}
	const want = "729e8a3e-b9ca-4182-9d9e-20d2b4304fb4"

	if got := cell(u); got != want {
		t.Errorf("CSV cell uuid: got %q want %q", got, want)
	}
	if got := jsonValue(u); got != want {
		t.Errorf("JSON value uuid: got %v want %q", got, want)
	}
}

func TestWriteJSON(t *testing.T) {
	var b strings.Builder
	cols := []string{"id", "name"}
	rows := [][]any{{int64(1), "alpha"}}
	if err := WriteJSON(&b, cols, rows); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{`"id": 1`, `"name": "alpha"`} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON missing %q:\n%s", want, out)
		}
	}
}

func TestDisambiguateColumns(t *testing.T) {
	// Простые дубликаты получают суффиксы _N.
	got := disambiguateColumns([]string{"id", "id", "id"})
	want := []string{"id", "id_2", "id_3"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dup %d: got %q want %q (%v)", i, got[i], want[i], got)
		}
	}
	// Сгенерированный "id_2" не должен совпадать с реальной колонкой "id_2":
	// все ключи уникальны, ни одна колонка не теряется.
	keys := disambiguateColumns([]string{"id", "id", "id_2"})
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			t.Errorf("duplicate key %q in %v — a column would be dropped", k, keys)
		}
		seen[k] = true
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 unique keys, got %v", keys)
	}
}

func TestWriteJSONDuplicateColumnsPreserved(t *testing.T) {
	// Две колонки "id" и реальная "id_2": все три значения сохраняются.
	var b strings.Builder
	cols := []string{"id", "id", "id_2"}
	rows := [][]any{{int64(1), int64(2), int64(3)}}
	if err := WriteJSON(&b, cols, rows); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{": 1", ": 2", ": 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("a duplicate-named column value was dropped (missing %q):\n%s", want, out)
		}
	}
}

func TestCSVFormulaInjection(t *testing.T) {
	var b strings.Builder
	cols := []string{"v"}
	rows := [][]any{
		{"=cmd|' /C calc'!A1"}, // формула → нейтрализуется
		{"+1+2"},               // похоже на формулу
		{"@SUM(A1)"},
		{"-5"},     // настоящее отрицательное число → без изменений
		{"3.14"},   // число → без изменений
		{"normal"}, // текст → без изменений
	}
	if err := WriteCSV(&b, cols, rows); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"'=cmd", "'+1+2", "'@SUM(A1)"} {
		if !strings.Contains(out, want) {
			t.Errorf("formula not neutralized, missing %q:\n%s", want, out)
		}
	}
	// Настоящее число не должно получать префикс-кавычку.
	if strings.Contains(out, "'-5") {
		t.Errorf("negative number should not be escaped:\n%s", out)
	}
}

func TestCSVFormulaInjectionLeadingSpace(t *testing.T) {
	// Ведущий пробел не должен прятать формулу: процессоры обрезают пробелы и
	// трактуют " =cmd()" как формулу. Префикс ' добавляется к ИСХОДНОЙ ячейке.
	var b strings.Builder
	cols := []string{"v"}
	rows := [][]any{
		{" =cmd()"},     // пробел перед = → всё равно формула
		{"\t=cmd()"},    // ведущая табуляция → формула (старое поведение)
		{"   @SUM(A1)"}, // несколько пробелов перед @
		{"  -5"},        // пробелы перед числом → числом и остаётся
		{"  hi"},        // обычный текст с отступом → без изменений
	}
	if err := WriteCSV(&b, cols, rows); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"' =cmd()", "'\t=cmd()", "'   @SUM(A1)"} {
		if !strings.Contains(out, want) {
			t.Errorf("leading-space formula not neutralized, missing %q:\n%s", want, out)
		}
	}
	// Число с ведущими пробелами не должно экранироваться.
	if strings.Contains(out, "'  -5") {
		t.Errorf("padded negative number should not be escaped:\n%s", out)
	}
	// Обычный текст с отступом не трогаем.
	if strings.Contains(out, "'  hi") {
		t.Errorf("indented plain text should not be escaped:\n%s", out)
	}
}

func TestCSVSafeLeadingSpaceUnit(t *testing.T) {
	cases := []struct{ in, want string }{
		{" =cmd()", "' =cmd()"},
		{"\t=cmd()", "'\t=cmd()"},
		{"\r=x", "'\r=x"},
		{"  @x", "'  @x"},
		{"  -5", "  -5"},     // число (со значимым -5) — не трогаем
		{"  3.14", "  3.14"}, // число — не трогаем
		{"   ", "   "},       // только пробелы, нет значимого символа
		{" plain", " plain"},
	}
	for _, c := range cases {
		if got := csvSafe(c.in); got != c.want {
			t.Errorf("csvSafe(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWriteCSVRowWidthMismatch(t *testing.T) {
	// Рассинхрон длины строки и числа колонок — явная ошибка, а не тихая порча.
	cols := []string{"a", "b", "c"}
	var b strings.Builder
	if err := WriteCSV(&b, cols, [][]any{{1, 2}}); err == nil {
		t.Errorf("WriteCSV: expected error on short row, got nil (output=%q)", b.String())
	}
	b.Reset()
	if err := WriteCSV(&b, cols, [][]any{{1, 2, 3, 4}}); err == nil {
		t.Errorf("WriteCSV: expected error on long row, got nil (output=%q)", b.String())
	}
}

func TestWriteJSONRowWidthMismatch(t *testing.T) {
	cols := []string{"a", "b", "c"}
	var b strings.Builder
	if err := WriteJSON(&b, cols, [][]any{{1, 2}}); err == nil {
		t.Errorf("WriteJSON: expected error on short row, got nil (output=%q)", b.String())
	}
	b.Reset()
	if err := WriteJSON(&b, cols, [][]any{{1, 2, 3, 4}}); err == nil {
		t.Errorf("WriteJSON: expected error on long row, got nil (output=%q)", b.String())
	}
}

func TestCSVStreamRowWidthMismatch(t *testing.T) {
	var b strings.Builder
	sw, err := NewCSVStream(&b, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.WriteRow([]any{1}); err == nil {
		t.Error("csvStream.WriteRow: expected error on short row, got nil")
	}
	if err := sw.WriteRow([]any{1, 2, 3}); err == nil {
		t.Error("csvStream.WriteRow: expected error on long row, got nil")
	}
}

func TestJSONStreamRowWidthMismatch(t *testing.T) {
	var b strings.Builder
	sw, err := NewJSONStream(&b, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.WriteRow([]any{1}); err == nil {
		t.Error("jsonStream.WriteRow: expected error on short row, got nil")
	}
	// Ошибка зафиксирована (sticky) — последующие записи и Close её возвращают.
	if err := sw.WriteRow([]any{1, 2}); err == nil {
		t.Error("jsonStream.WriteRow: expected sticky error after mismatch, got nil")
	}
	if err := sw.Close(); err == nil {
		t.Error("jsonStream.Close: expected sticky error after mismatch, got nil")
	}
}

func TestJSONDuplicateColumns(t *testing.T) {
	var b strings.Builder
	cols := []string{"id", "id", "id"}
	rows := [][]any{{int64(1), int64(2), int64(3)}}
	if err := WriteJSON(&b, cols, rows); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{`"id":`, `"id_2":`, `"id_3":`} {
		if !strings.Contains(out, want) {
			t.Errorf("duplicate column not preserved, missing %q:\n%s", want, out)
		}
	}
}

// TestCSVStreamMatchesBatch проверяет, что потоковая запись CSV даёт тот же
// результат, что и WriteCSV для тех же строк.
func TestCSVStreamMatchesBatch(t *testing.T) {
	cols := []string{"shard", "id", "name"}
	rows := [][]any{
		{"shard_0", int64(1), "alpha"},
		{"shard_1", int64(2), nil},
		{"shard_1", int64(3), "=danger"}, // ячейка с формулой экранируется
	}
	var batch strings.Builder
	if err := WriteCSV(&batch, cols, rows); err != nil {
		t.Fatal(err)
	}
	var stream strings.Builder
	sw, err := NewCSVStream(&stream, cols)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := sw.WriteRow(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	if batch.String() != stream.String() {
		t.Errorf("streamed CSV != batch CSV\nbatch:\n%s\nstream:\n%s", batch.String(), stream.String())
	}
	if sw.Rows() != len(rows) {
		t.Errorf("Rows() = %d, want %d", sw.Rows(), len(rows))
	}
}

// TestJSONStreamValidAndComplete проверяет, что потоковая запись JSON выдаёт
// валидный массив со всеми строками.
func TestJSONStreamValidAndComplete(t *testing.T) {
	cols := []string{"id", "name"}
	rows := [][]any{{int64(1), "a"}, {int64(2), "b"}, {int64(3), nil}}
	var buf strings.Builder
	sw, err := NewJSONStream(&buf, cols)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := sw.WriteRow(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &got); err != nil {
		t.Fatalf("streamed JSON is invalid: %v\n%s", err, buf.String())
	}
	if len(got) != 3 {
		t.Errorf("expected 3 objects, got %d: %s", len(got), buf.String())
	}
	if got[0]["name"] != "a" || got[2]["name"] != nil {
		t.Errorf("unexpected JSON content: %v", got)
	}
}
