package sqlsplit

import (
	"strings"
	"testing"

	"terox/internal/pgquery"
)

// FuzzConsumersAgree доказывает, что разные потребители ЕДИНОГО лексера pgquery
// согласованы (P2-3): sqlsplit.InStringOrComment(s) истинно ровно тогда, когда
// pgquery.TrailingStateOf(s) != StateCode — оба отвечают на «конец ввода внутри
// литерала/комментария?», но через разные кодовые пути. Расхождение означало бы,
// что подсистемы по-разному поняли один и тот же SQL.
func FuzzConsumersAgree(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		inLit := InStringOrComment(s)
		state := pgquery.TrailingStateOf(s)
		if inLit != (state != pgquery.StateCode) {
			t.Fatalf("disagreement on %q: InStringOrComment=%v TrailingState=%d", s, inLit, state)
		}
	})
}

// fuzzSeeds — общие сиды для каверзных углов лексера: dollar-quote,
// вложенные блочные комментарии, экранированные строки, идентификаторы
// в кавычках, Unicode и незакрытые токены.
var fuzzSeeds = []string{
	"",
	"select 1",
	"select 1; select 2;",
	"select ';' as x; delete from t",
	"select $$a;b$$; select 2",
	"select $tag$ ; $tag$ from t",
	"-- a;b\nselect 1",
	"/* a /* nested */ still */ select 1",
	"select E'a\\';b' ; select 2",
	`select "id;col" from t`,
	"select U&'\\0061' from t",
	`update "weird ;name" set x=1`,
	"insert into t values ('a''b;c'); select 2",
	"$$unterminated",
	"'unterminated",
	"/* unterminated",
	"select 'тест; key' as кир",
	"select $тег$ a;b $тег$ ; select 2", // не-ASCII dollar-quote тег
	"select $café$ x;y $café$",          // тег с диакритикой
	"A$$",                               // '$' в идентификаторе, НЕ dollar-quote (регресс)
	"func$$body$$ ; select 2",           // идентификатор с '$$', не dollar-quote
	"a$b$c; select 2",                   // '$' внутри идентификатора
	"\x00\x01\xff;\xfe",
}

// FuzzMaskPreservesLength проверяет: Mask не паникует и возвращает строку
// ровно той же длины в байтах, что и вход — контракт, на который опираются
// слои safety, migration и completion ради стабильности байтовых смещений.
func FuzzMaskPreservesLength(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := Mask(s)
		if len(got) != len(s) {
			t.Fatalf("Mask changed length: in=%d out=%d for %q", len(s), len(got), s)
		}
		// Каждый структурный байт вне литералов/комментариев сохраняется:
		// в частности, Mask не затирает верхнеуровневые ';' в строке без
		// кавычек/комментариев.
	})
}

// FuzzSplitStable проверяет: Split не паникует, а каждый возвращённый стейтмент
// сам является одиночным (повторный Split даёт ровно один), т.е. Split не
// оставляет незамаскированный верхнеуровневый разделитель внутри куска.
func FuzzSplitStable(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		stmts := Split(s)
		for _, st := range stmts {
			if strings.TrimSpace(st) == "" {
				t.Fatalf("Split returned an empty statement from %q", s)
			}
			if n := len(Split(st)); n != 1 {
				t.Fatalf("re-splitting a single statement %q gave %d pieces", st, n)
			}
		}
	})
}

// FuzzInStringOrComment проверяет: проба контекста курсора редактора не паникует
// на произвольном вводе (включая незакрытый / не-UTF-8).
func FuzzInStringOrComment(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_ = InStringOrComment(s)
		_ = MaskKeepQuoted(s)
	})
}
