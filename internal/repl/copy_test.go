package repl

import (
	"bytes"
	"strings"
	"testing"
)

func TestFindCopyDirection(t *testing.T) {
	cases := []struct {
		in  string
		dir string
	}{
		{"users to users.csv", "to"},
		{"users from users.csv", "from"},
		{"(select * from t) to f.csv", "to"},     // to после закрывающей скобки
		{"public.t(a,b) from f.csv csv", "from"}, // from после )
		{"users", ""},                            // нет направления
		{"(select 1 from t where x to y)", ""},   // to внутри скобок не считается
		{"t\nfrom file.csv", "from"},             // многострочный: \n как разделитель
		{"t\rto file.csv", "to"},                 // CR как разделитель
		{"users\nto\nusers.csv", "to"},           // \n с обеих сторон ключевого слова
		{"public.t(a,b)\nfrom f.csv", "from"},    // from после ) на новой строке
	}
	for _, c := range cases {
		idx, dir := findCopyDirection(c.in)
		if dir != c.dir {
			t.Errorf("findCopyDirection(%q) dir=%q, want %q (idx %d)", c.in, dir, c.dir, idx)
		}
	}
}

func TestValidateCopySource(t *testing.T) {
	ok := []string{"users", "public.users", "users(a,b)", "schema.t ( a , b )"}
	for _, s := range ok {
		if err := validateCopySource(s, false); err != nil {
			t.Errorf("validateCopySource(%q) table: unexpected error %v", s, err)
		}
	}
	// Инъекция в табличном источнике отклоняется.
	bad := []string{
		"users TO PROGRAM 'rm -rf /'",
		"users; drop table x",
		"users to stdout) ,",
		"t TO '/etc/passwd'",
	}
	for _, s := range bad {
		if err := validateCopySource(s, false); err == nil {
			t.Errorf("validateCopySource(%q) should be rejected (injection)", s)
		}
	}
	// Запрос разрешён только в режиме TO и только read-only/сбалансированный.
	if err := validateCopySource("(select * from t)", true); err != nil {
		t.Errorf("balanced read query should be ok: %v", err)
	}
	if err := validateCopySource("(select 1) TO PROGRAM 'x'", true); err == nil {
		t.Error("trailing text after query ) must be rejected")
	}
	if err := validateCopySource("(delete from t returning *)", true); err == nil {
		t.Error("writing query in COPY TO must be rejected")
	}
	if err := validateCopySource("(select 1)", false); err == nil {
		t.Error("COPY FROM must reject a query source")
	}
}

func TestValidateCopySourceLiteralAware(t *testing.T) {
	// Скобка ВНУТРИ строкового литерала не должна обманывать счётчик баланса:
	// после маскирования реальная закрывающая ) — раньше конца, значит trailing text.
	if err := validateCopySource("(select '(' from t) injected )", true); err == nil {
		t.Error("paren inside a string literal must not fool the balance check (expected trailing-text error)")
	}
	// Нормальный запрос со скобкой-литералом, корректно завершённый, проходит.
	if err := validateCopySource("(select '(' as x from t)", true); err != nil {
		t.Errorf("balanced query with a literal paren should be ok: %v", err)
	}
}

func TestFindCopyDirectionLiteralAware(t *testing.T) {
	// 'to' внутри литерала не считается направлением; реальное to — после ).
	idx, dir := findCopyDirection("(select 'a to b' from t) to f.csv")
	if dir != "to" {
		t.Fatalf("expected to-direction, got %q", dir)
	}
	// Индекс должен указывать на настоящее ' to ' после закрывающей скобки.
	if idx <= strings.Index("(select 'a to b' from t) to f.csv", ")") {
		t.Errorf("direction index %d should be after the real ')'", idx)
	}
}

func TestCopyFormatOption(t *testing.T) {
	if o, _ := copyFormatOption(""); !strings.Contains(o, "csv") || !strings.Contains(o, "HEADER") {
		t.Errorf("default format = %q, want csv+HEADER", o)
	}
	if o, _ := copyFormatOption("text"); !strings.Contains(o, "text") {
		t.Errorf("text format = %q", o)
	}
	if o, _ := copyFormatOption("tsv"); !strings.Contains(o, "text") {
		t.Errorf("tsv should map to text, got %q", o)
	}
	if _, err := copyFormatOption("binary"); err == nil {
		t.Error("unknown format should error")
	}
}

// TestParseCopyTail проверяет разбор хвоста: путь + опциональные format/force.
func TestParseCopyTail(t *testing.T) {
	file, format, force, err := parseCopyTail([]string{"out.csv", "csv", "force"})
	if err != nil || file != "out.csv" || format != "csv" || !force {
		t.Errorf("parseCopyTail = (%q,%q,%v,%v), want (out.csv,csv,true,nil)", file, format, force, err)
	}
	if _, _, _, err := parseCopyTail(nil); err == nil {
		t.Error("missing file path must error")
	}
	if _, _, _, err := parseCopyTail([]string{"f.csv", "csv", "tsv"}); err == nil {
		t.Error("duplicate format token must error")
	}
}

// TestCopyForceRejectedForFrom: force осмыслен только для TO (перезапись файла);
// для FROM это no-op, который мог бы быть принят за partial import/bypass — ошибка.
func TestCopyForceRejectedForFrom(t *testing.T) {
	var buf bytes.Buffer
	r := &REPL{out: &buf}
	err := r.doCopy("\\copy t from f.csv csv force")
	if err == nil || !strings.Contains(err.Error(), "force is only valid for COPY TO") {
		t.Errorf("force on COPY FROM must be rejected, got %v", err)
	}
	// force на TO не должно падать на ЭТОЙ проверке (упадёт позже — нет выбранного шарда).
	err = r.doCopy("\\copy t to f.csv csv force")
	if err != nil && strings.Contains(err.Error(), "force is only valid") {
		t.Errorf("force on COPY TO must be allowed past the force check: %v", err)
	}
}
