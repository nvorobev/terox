package pgquery

import "testing"

func kinds(toks []Token) []Kind {
	out := make([]Kind, len(toks))
	for i, t := range toks {
		out[i] = t.Kind
	}
	return out
}

func TestLexBasic(t *testing.T) {
	toks := Lex("select a from t")
	want := []Kind{Word, Whitespace, Word, Whitespace, Word, Whitespace, Word}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens %v, want %d", len(toks), kinds(toks), len(want))
	}
	for i := range want {
		if toks[i].Kind != want[i] {
			t.Errorf("token[%d] kind=%d, want %d", i, toks[i].Kind, want[i])
		}
	}
}

func TestLexLiterals(t *testing.T) {
	cases := []struct {
		in   string
		kind Kind
	}{
		{"'hello'", String},
		{"E'a\\'b'", String},         // escape-строка с экранированием кавычки слешем
		{`U&'\0061bc'`, String},      // unicode-escape строка
		{`"col"`, QuotedIdent},       // кавыченный идентификатор
		{`U&"d\0061t"`, QuotedIdent}, // unicode-escape идентификатор
		{"$$body;here$$", DollarString},
		{"$tag$ a;b $tag$", DollarString},
		{"$тег$x$тег$", DollarString}, // не-ASCII тег
		{"$1", Param},
		{"42", Number},
		{"3.14", Number},
		{"1e9", Number},
		{"col", Word},
		{"café", Word}, // не-ASCII идентификатор
		{"-- comment", Comment},
		{"/* a /* nested */ still */", Comment},
		{">=", Operator},
	}
	for _, c := range cases {
		toks := Lex(c.in)
		if len(toks) != 1 {
			t.Errorf("Lex(%q) = %d tokens %v, want 1", c.in, len(toks), kinds(toks))
			continue
		}
		if toks[0].Kind != c.kind {
			t.Errorf("Lex(%q) kind=%d, want %d", c.in, toks[0].Kind, c.kind)
		}
		if toks[0].Text != c.in {
			t.Errorf("Lex(%q) text=%q, want whole input", c.in, toks[0].Text)
		}
	}
}

func TestLexIncomplete(t *testing.T) {
	cases := []struct {
		in    string
		state TrailingState
	}{
		{"select 'unterminated", StateString},
		{"select $tag$ open", StateString},
		{`select "qident`, StateQIdent},
		{`select U&"qid`, StateQIdent},
		{"select /* open", StateComment},
		{"-- line", StateComment},
		{"select 1", StateCode},
		{"", StateCode},
		{"select 'closed'", StateCode},
	}
	for _, c := range cases {
		if got := TrailingStateOf(c.in); got != c.state {
			t.Errorf("TrailingStateOf(%q) = %d, want %d", c.in, got, c.state)
		}
		// Незавершённый ввод заканчивается токеном Incomplete (кроме чистого кода/комментов).
		toks := Lex(c.in)
		if c.state == StateString || c.state == StateQIdent {
			if len(toks) == 0 || toks[len(toks)-1].Kind != Incomplete {
				t.Errorf("Lex(%q) last token should be Incomplete, got %v", c.in, kinds(toks))
			}
		}
	}
}

// TestLexOperatorAdjacentComment: набор оператора не должен заглатывать начало
// комментария (-- или /*) сразу за оператором. Иначе тело комментария «протекает»
// живыми токенами и Lex расходится с байтовыми сканерами Split/Mask.
func TestLexOperatorAdjacentComment(t *testing.T) {
	// /* вплотную за оператором: a(Word) =(Operator) /* x */(Comment) b(Word).
	toks := Lex("a=/* x */b")
	want := []Kind{Word, Operator, Comment, Word}
	if len(toks) != len(want) {
		t.Fatalf("Lex(a=/* x */b) = %v, want %v", kinds(toks), want)
	}
	for i, k := range want {
		if toks[i].Kind != k {
			t.Fatalf("Lex(a=/* x */b) kinds = %v, want %v", kinds(toks), want)
		}
	}
	if toks[1].Text != "=" || toks[2].Text != "/* x */" {
		t.Fatalf("operator/block-comment split wrong: %q / %q", toks[1].Text, toks[2].Text)
	}

	// -- вплотную за оператором: a(Word) <(Operator) --c(Comment).
	toks = Lex("a<--c")
	if len(toks) != 3 || toks[0].Kind != Word || toks[1].Kind != Operator || toks[2].Kind != Comment {
		t.Fatalf("Lex(a<--c) = %v, want [Word Operator Comment]", kinds(toks))
	}
	if toks[1].Text != "<" || toks[2].Text != "--c" {
		t.Fatalf("operator/line-comment split wrong: %q / %q", toks[1].Text, toks[2].Text)
	}

	// Незакрытый блочный комментарий сразу за оператором → StateComment, чтобы
	// автодополнение SQL-объектов внутри комментария подавлялось.
	if got := TrailingStateOf("select * from t where a=/* still comment "); got != StateComment {
		t.Errorf("TrailingStateOf inside operator-adjacent comment = %d, want StateComment(%d)", got, StateComment)
	}

	// Обычные многосимвольные операторы по-прежнему один токен (нет -- или /* внутри).
	for _, op := range []string{">=", "<=", "<>", "!=", "||", "->", "->>", "<->", "@>", "&&"} {
		ts := Lex(op)
		if len(ts) != 1 || ts[0].Kind != Operator || ts[0].Text != op {
			t.Errorf("Lex(%q) = %v, want single Operator", op, kinds(ts))
		}
	}
}

func TestLexParamVsDollarTag(t *testing.T) {
	// $1$ — это $1 (param) и '$' (operator), а не открытие dollar-quote.
	toks := Lex("$1$")
	if len(toks) != 2 || toks[0].Kind != Param || toks[1].Kind != Operator {
		t.Fatalf("Lex($1$) = %v, want [Param, Operator]", kinds(toks))
	}
}

func TestPrimitives(t *testing.T) {
	if tag, ok := DollarTag("$tag$x", 0); !ok || tag != "$tag$" {
		t.Errorf("DollarTag = %q,%v", tag, ok)
	}
	if _, ok := DollarTag("$1$", 0); ok {
		t.Error("$1$ must not be a dollar tag")
	}
	if !IsEscapeStringStart("E'x'", 1) {
		t.Error("E'x' should be an escape string at index 1")
	}
	if IsEscapeStringStart("the'x'", 3) {
		t.Error("the'x' — quote after identifier tail is not an escape string")
	}
	if !IsUAmpStart(`U&"x"`, 0) || !IsUAmpStart(`u&'x'`, 0) {
		t.Error("U&\"/u&' should be UAmp starts")
	}
	if IsUAmpStart(`fU&"x"`, 1) {
		t.Error("U& preceded by an identifier byte is not a UAmp start")
	}
	end, ok := ScanQuoted("'a''b'", 0, false)
	if !ok || end != 6 {
		t.Errorf("ScanQuoted doubled-quote = %d,%v, want 6,true", end, ok)
	}
	end, ok = ScanBlockComment("/* a /* b */ c */tail", 0)
	if !ok || end != 17 {
		t.Errorf("ScanBlockComment nested = %d,%v, want 17,true", end, ok)
	}
	dec, _ := DecodeUEscaped(`"d\0061t"`, 0)
	if dec != "dat" {
		t.Errorf("DecodeUEscaped = %q, want dat", dec)
	}
}
