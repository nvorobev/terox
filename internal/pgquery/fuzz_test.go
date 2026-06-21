package pgquery

import "testing"

var fuzzSeeds = []string{
	"",
	"select 1",
	"select 'a;b'; select 2",
	"select $$a;b$$; select 2",
	"select $tag$ ; $tag$ from t",
	"select $тег$ x;y $тег$",
	"E'a\\';b'",
	`U&'\0061' U&"d\0061t"`,
	"-- a;b\nselect 1",
	"/* a /* nested */ still */ select 1",
	`select "id;col" from t`,
	"$1 + $2",
	"select * from t where x >= 1 and y <> 2",
	"unterminated 'string",
	"/* unterminated",
	`"unterminated qident`,
	"$open$ no close",
	"A$$",          // '$' в идентификаторе, не dollar-quote
	"func$$body$$", // идентификатор с '$$'
	"\x00\x01\xff;\xfe",
	"привет; мир",
	"a=/* c;d */b",   // комментарий вплотную за оператором
	"x<--y;z\nw",     // строчный комментарий вплотную за оператором
	"select 1=/*x*/", // незакрытого нет, но оператор у /*
}

// FuzzLexCoversAllBytes: токены покрывают КАЖДЫЙ байт без пропусков/наложений, их
// конкатенация равна входу, и Text == s[Start:End]. Гарантирует, что любой
// потребитель может восстановить исходник из токенов.
func FuzzLexCoversAllBytes(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		toks := Lex(s)
		if len(s) == 0 {
			if len(toks) != 0 {
				t.Fatalf("empty input should yield no tokens, got %d", len(toks))
			}
			return
		}
		pos := 0
		var b []byte
		for k, tk := range toks {
			if tk.Start != pos {
				t.Fatalf("token[%d] Start=%d, want %d (gap/overlap) in %q", k, tk.Start, pos, s)
			}
			if tk.End < tk.Start || tk.End > len(s) {
				t.Fatalf("token[%d] bad range [%d,%d] in %q", k, tk.Start, tk.End, s)
			}
			if tk.Text != s[tk.Start:tk.End] {
				t.Fatalf("token[%d] Text mismatch in %q", k, s)
			}
			b = append(b, tk.Text...)
			pos = tk.End
		}
		if pos != len(s) {
			t.Fatalf("tokens cover %d of %d bytes in %q", pos, len(s), s)
		}
		if string(b) != s {
			t.Fatalf("concatenated tokens != input for %q", s)
		}
	})
}

// FuzzTrailingStateStable: TrailingStateOf не паникует на произвольном вводе и
// всегда возвращает одно из определённых состояний (стабильность для редактора).
func FuzzTrailingStateStable(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		switch TrailingStateOf(s) {
		case StateCode, StateString, StateComment, StateQIdent:
		default:
			t.Fatalf("TrailingStateOf(%q) returned an undefined state", s)
		}
	})
}
