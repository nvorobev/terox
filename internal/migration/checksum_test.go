package migration

import "testing"

func TestChecksum(t *testing.T) {
	a := Checksum("CREATE TABLE t (id int);")
	// Детерминированность.
	if a != Checksum("CREATE TABLE t (id int);") {
		t.Error("checksum must be deterministic")
	}
	// Нормализация CRLF и хвостовых пробелов не меняет сумму.
	if a != Checksum("CREATE TABLE t (id int);  \r\n") {
		t.Error("trailing whitespace / CRLF should normalize to the same checksum")
	}
	// Любое содержательное изменение меняет сумму.
	if a == Checksum("CREATE TABLE t (id bigint);") {
		t.Error("different SQL must yield a different checksum")
	}
	// 64 hex-символа (sha256).
	if len(a) != 64 {
		t.Errorf("checksum length = %d, want 64", len(a))
	}
}

// TestChecksumLiteralWhitespace: значимый хвостовой пробел внутри строкового или
// dollar-quoted литерала ДОЛЖЕН менять сумму — иначе две семантически разных
// миграции дали бы коллизию (целостность ledger'а).
func TestChecksumLiteralWhitespace(t *testing.T) {
	// Пробел перед переводом строки ВНУТРИ строкового литерала значим.
	if Checksum("insert into t values ('abc \ndef');") == Checksum("insert into t values ('abc\ndef');") {
		t.Error("trailing space inside a string literal must change the checksum")
	}
	// То же для тела функции в dollar-quote.
	withSpace := "create function f() returns void as $$\n  select 1; \n$$ language sql;"
	noSpace := "create function f() returns void as $$\n  select 1;\n$$ language sql;"
	if Checksum(withSpace) == Checksum(noSpace) {
		t.Error("trailing space inside a dollar-quoted body must change the checksum")
	}
}
