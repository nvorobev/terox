package repl

import (
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"terox/internal/complete"
	"terox/internal/config"
)

func newTestEditor() *editorModel {
	cat := &complete.Catalog{
		SearchPath: []string{"public"},
		Schemas:    []string{"public", "items"},
		Relations: []complete.Relation{
			{Schema: "public", Name: "orders", Kind: "r"},
			{Schema: "public", Name: "order_items", Kind: "r"},
			{Schema: "items", Name: "products", Kind: "r"},
		},
		Keywords: []string{"select", "from", "where"},
		Shards:   1,
	}
	r := &REPL{out: io.Discard, suggest: true, cfg: &config.Config{}}
	r.catalog = cat
	r.comp = newCompleter(r)
	return &editorModel{r: r, comp: r.comp}
}

func typeStr(m *editorModel, s string) {
	for _, ch := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ch}})
	}
}

func press(m *editorModel, t tea.KeyType) { m.Update(tea.KeyMsg{Type: t}) }

func TestEditorLiveMenu(t *testing.T) {
	m := newTestEditor()
	typeStr(m, "select * from o")
	if !m.menuOpen {
		t.Fatalf("menu should open live as you type; cands=%v", m.cands)
	}
	found := map[string]bool{}
	for _, c := range m.cands {
		found[c] = true
	}
	if !found["orders"] || !found["order_items"] {
		t.Errorf("live menu should show orders/order_items; got %v", m.cands)
	}
	// Таблица из другой схемы не должна появляться без квалификатора (навигация от схемы).
	if found["products"] {
		t.Errorf("bare other-schema table leaked into menu: %v", m.cands)
	}
}

func TestEditorTabAccepts(t *testing.T) {
	m := newTestEditor()
	typeStr(m, "select * from order_i")
	if !m.menuOpen {
		t.Fatal("menu should be open")
	}
	press(m, tea.KeyTab)
	if m.line() != "select * from order_items" {
		t.Errorf("Tab should complete to order_items; got %q", m.line())
	}
}

func TestEditorMenuNavAndEsc(t *testing.T) {
	m := newTestEditor()
	typeStr(m, "select * from o")
	if len(m.cands) < 2 {
		t.Fatalf("need >=2 candidates; got %v", m.cands)
	}
	press(m, tea.KeyDown)
	if m.sel != 1 {
		t.Errorf("Down should move selection to 1; got %d", m.sel)
	}
	press(m, tea.KeyEsc)
	if m.menuOpen {
		t.Error("Esc should close the menu")
	}
	typeStr(m, "r")
	if !m.menuOpen {
		t.Error("typing after Esc should reopen the menu")
	}
}

func TestEditorSubmitAndControls(t *testing.T) {
	m := newTestEditor()
	typeStr(m, "select 1;") // завершённый оператор → Enter отправляет
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.done || m.submitted != "select 1;" {
		t.Errorf("Enter should submit a complete statement; done=%v submitted=%q", m.done, m.submitted)
	}

	empty := newTestEditor()
	empty.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if !empty.eof {
		t.Error("Ctrl-D on an empty line should signal EOF")
	}

	busy := newTestEditor()
	typeStr(busy, "abc")
	busy.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !busy.interrupted {
		t.Error("Ctrl-C should interrupt")
	}
}

func TestEditorHistory(t *testing.T) {
	m := newTestEditor()
	m.hist = []string{"select 1", "select 2"}
	m.histIdx = len(m.hist)
	press(m, tea.KeyUp)
	if m.line() != "select 2" {
		t.Errorf("Up should recall the most recent history; got %q", m.line())
	}
	press(m, tea.KeyUp)
	if m.line() != "select 1" {
		t.Errorf("Up again should go older; got %q", m.line())
	}
	press(m, tea.KeyDown)
	if m.line() != "select 2" {
		t.Errorf("Down should go newer; got %q", m.line())
	}
}

func TestEditorSwitchCommand(t *testing.T) {
	dir := t.TempDir()
	cfg, _ := config.Load(dir + "/config.yaml") // привязан к пути, файла ещё нет
	r := &REPL{out: io.Discard, cfg: cfg, histPath: dir + "/hist"}

	if err := r.doEditor([]string{"tea"}); err != nil {
		t.Fatal(err)
	}
	if !r.useTeaEditor || cfg.Editor != "tea" {
		t.Errorf("'\\editor tea' should select+persist tea; use=%v cfg=%q", r.useTeaEditor, cfg.Editor)
	}
	if err := r.doEditor([]string{"readline"}); err != nil {
		t.Fatal(err)
	}
	if r.useTeaEditor || cfg.Editor != "readline" {
		t.Errorf("'\\editor readline' should select+persist readline; use=%v cfg=%q", r.useTeaEditor, cfg.Editor)
	}
	if err := r.doEditor([]string{"bogus"}); err == nil {
		t.Error("unknown editor should error")
	}
}

func TestEditorNoGhostOnEmpty(t *testing.T) {
	m := newTestEditor()
	// ghostHint на пустом вводе всё равно подбирает ключевое слово верхнего уровня...
	g, _ := m.comp.ghostHint("")
	// ...но пустой редактор его не отрисовывает.
	if g != "" && strings.Contains(m.View(), g) {
		t.Errorf("empty editor must not show a ghost %q; View=%q", g, m.View())
	}
	// При наличии ввода подсказка-призрак может появиться.
	typeStr(m, "sel")
	if g2, _ := m.comp.ghostHint("sel"); g2 == "" {
		t.Errorf("expected a ghost after typing 'sel'")
	}
}

func TestEditorNoAutoSuggestAfterSpace(t *testing.T) {
	m := newTestEditor()
	typeStr(m, "select * from ") // пробел в конце → токен не вводится
	if m.menuOpen {
		t.Errorf("menu must NOT auto-open after a space; cands=%v", m.cands)
	}
	if g, _ := m.comp.ghostHint(m.line()); g != "" {
		t.Errorf("ghost must be empty after a space; got %q", g)
	}
	// Явный Tab открывает меню (таблицы в области видимости).
	press(m, tea.KeyTab)
	if !m.menuOpen {
		t.Errorf("Tab should force the menu open after a space; cands=%v", m.cands)
	}
	// Ввод буквы возобновляет авто-подсказку.
	typeStr(m, "o")
	if !m.menuOpen {
		t.Errorf("typing a token should auto-open the menu; cands=%v", m.cands)
	}
	// Точка-квалификатор тоже срабатывает (переход к схеме), это не пробел.
	m2 := newTestEditor()
	typeStr(m2, "select * from items.")
	if !m2.menuOpen {
		t.Errorf("'schema.' should auto-open the menu; cands=%v", m2.cands)
	}
}

func TestEditorClearScreen(t *testing.T) {
	m := newTestEditor()
	typeStr(m, "select 1")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	if cmd == nil {
		t.Error("Ctrl-L should issue a clear-screen command")
	}
	if m.line() != "select 1" || m.done {
		t.Errorf("Ctrl-L must keep the input and not submit; line=%q done=%v", m.line(), m.done)
	}
}

func TestEditorCursorEditing(t *testing.T) {
	m := newTestEditor()
	typeStr(m, "selct")
	press(m, tea.KeyLeft)
	press(m, tea.KeyLeft)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if m.line() != "select" {
		t.Errorf("mid-line insert failed; got %q", m.line())
	}
	press(m, tea.KeyBackspace)
	if m.line() != "selct" {
		t.Errorf("backspace at cursor failed; got %q", m.line())
	}
}

func TestEditorNoSuggestAfterDelimiters(t *testing.T) {
	for _, in := range []string{"select 1;", "select id,", "select * ", "select id, total from t)"} {
		m := newTestEditor()
		typeStr(m, in)
		if m.menuOpen {
			t.Errorf("%q must not auto-open the menu (ends on a delimiter); cands=%v", in, m.cands)
		}
		if g, _ := m.comp.ghostHint(m.line()); g != "" {
			t.Errorf("%q must not show a ghost; got %q", in, g)
		}
	}
}

func TestGhostNoLongerAlternativeOnExactMatch(t *testing.T) {
	cat := &complete.Catalog{
		SearchPath: []string{"public"},
		Schemas:    []string{"public", "statistics"},
		Relations: []complete.Relation{
			{Schema: "statistics", Name: "shop_items_cnt", Kind: "r"},
			{Schema: "statistics", Name: "shop_items_cnt_aggregate", Kind: "r"},
		},
		Keywords: []string{"select", "from"},
		Shards:   1,
	}
	r := &REPL{out: io.Discard, suggest: true, cfg: &config.Config{}}
	r.catalog = cat
	r.comp = newCompleter(r)
	// Введено полное имя таблицы → призрак не предлагает более длинный вариант.
	if g, _ := r.comp.ghostHint("select * from statistics.shop_items_cnt"); g != "" {
		t.Errorf("exact table name must not ghost a longer alternative; got %q", g)
	}
	// Частичное имя по-прежнему даёт призрак-дополнение.
	if g, _ := r.comp.ghostHint("select * from statistics.shop_items_c"); g == "" {
		t.Error("a partial name should still produce a ghost")
	}
}

func TestEditorGhostMatchesTab(t *testing.T) {
	m := newTestEditor()
	typeStr(m, "select * from o")
	if !m.menuOpen || len(m.full) == 0 {
		t.Fatalf("menu should be open with candidates; cands=%v", m.cands)
	}
	// Призрак показывает суффикс выбранной строки; Tab вставляет именно его.
	want := m.full[m.sel]
	line0 := m.line()
	press(m, tea.KeyTab)
	if got := m.line()[len(line0):]; got != want {
		t.Errorf("Tab must insert the ghosted selection %q; inserted %q", want, got)
	}

	// Стрелка вниз меняет выбор, и призрак следует за ним.
	m2 := newTestEditor()
	typeStr(m2, "select * from o")
	g0 := m2.full[m2.sel]
	press(m2, tea.KeyDown)
	g1 := m2.full[m2.sel]
	if g0 == g1 {
		t.Errorf("Down should move the selection (ghost): both %q", g0)
	}
}

func TestEditorEnterAcceptsWhenNavigated(t *testing.T) {
	// Завершённый оператор (";") + Enter — отправка (без принятия авто-подсказки).
	m := newTestEditor()
	typeStr(m, "select 1;")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.done || m.submitted != "select 1;" {
		t.Errorf("Enter on a complete statement should submit; done=%v sub=%q", m.done, m.submitted)
	}

	// Войти в меню через Down, затем Enter принимает выбор (без отправки).
	m2 := newTestEditor()
	typeStr(m2, "select * from o")
	press(m2, tea.KeyDown) // режим навигации по меню, выбор индекса 1
	want := m2.cands[m2.sel]
	line0 := m2.line()
	m2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m2.done {
		t.Error("Enter while navigating the menu must accept, not submit")
	}
	if got := m2.line()[len(line0)-len("o"):]; got != want {
		t.Errorf("Enter should accept the highlighted %q; line tail %q", want, got)
	}
}

func TestEditorCursorRendersMidLine(t *testing.T) {
	m := newTestEditor()
	typeStr(m, "select")
	press(m, tea.KeyLeft)
	press(m, tea.KeyLeft) // курсор между "sele|ct"
	out := m.renderInput(true)
	// Все символы сохранены и в порядке (курсор не теряет текст).
	if !strings.Contains(out, "sele") || !strings.Contains(out, "ct") {
		t.Errorf("mid-line render must keep all text; got %q", out)
	}
	// В конце строки текст присутствует (с ячейкой блочного курсора после него).
	m2 := newTestEditor()
	typeStr(m2, "ab")
	if got := m2.renderInput(true); !strings.Contains(got, "ab") {
		t.Errorf("end-of-line render should contain the text; got %q", got)
	}
}

func TestEditorReverseSearch(t *testing.T) {
	m := newTestEditor()
	m.hist = []string{"select * from orders", "update items set x=1", "select count(*) from items"}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	if !m.searching {
		t.Fatal("Ctrl-R should enter reverse search")
	}
	typeStr(m, "count")
	if m.line() != "select count(*) from items" {
		t.Errorf("search 'count' should recall the matching entry; got %q", m.line())
	}
	// Enter принимает совпадение (выходит из поиска, без отправки).
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.searching || m.done {
		t.Errorf("Enter in search should accept, not submit; searching=%v done=%v", m.searching, m.done)
	}
	if m.line() != "select count(*) from items" {
		t.Errorf("accepted line wrong: %q", m.line())
	}

	// Ctrl-C отменяет и восстанавливает ввод до поиска.
	m2 := newTestEditor()
	m2.hist = []string{"select 1", "select 2"}
	typeStr(m2, "abc")
	m2.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	typeStr(m2, "select")
	m2.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if m2.searching || m2.interrupted {
		t.Errorf("Ctrl-C in search should cancel search, not interrupt the editor")
	}
	if m2.line() != "abc" {
		t.Errorf("Ctrl-C should restore the pre-search input 'abc'; got %q", m2.line())
	}
}

func TestConvertLayout(t *testing.T) {
	cases := map[string]string{
		"ыудусе":  "select",  // select на русской раскладке
		"ГЗВФеу":  "UPDAte",  // смешанный регистр по позиции
		" select": " select", // ASCII не трогается
		"шеуьы":   "items",   // имя таблицы
		"ы%":      "s%",      // не-буква проходит как есть
	}
	for in, want := range cases {
		if got := ConvertLayout(in); got != want {
			t.Errorf("ConvertLayout(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEditorAutoKeymap(t *testing.T) {
	m := newTestEditor()
	m.autoKeymap = true
	// Ввод "select" на русской раскладке приходит кириллицей → конвертируется.
	typeStr(m, "ыудусе") // позиции s-e-l-e-c-t
	if m.line() != "select" {
		t.Errorf("Cyrillic command should convert to 'select'; got %q", m.line())
	}

	// Внутри строкового литерала кириллица сохраняется как есть (реальное значение).
	m2 := newTestEditor()
	m2.autoKeymap = true
	typeStr(m2, "select * from t where name = '")
	typeStr(m2, "Иван") // внутри строки → остаётся кириллицей
	if !strings.Contains(m2.line(), "Иван") {
		t.Errorf("Cyrillic inside a '' string must be preserved; got %q", m2.line())
	}

	// Внутри идентификатора в двойных кавычках кириллица тоже сохраняется как есть.
	m2b := newTestEditor()
	m2b.autoKeymap = true
	typeStr(m2b, `select * from "`)
	typeStr(m2b, "Таблица")
	if !strings.Contains(m2b.line(), "Таблица") {
		t.Errorf(`Cyrillic inside a "" identifier must be preserved; got %q`, m2b.line())
	}

	// Выключено → нет конвертации.
	m3 := newTestEditor()
	m3.autoKeymap = false
	typeStr(m3, "ыудусе")
	if m3.line() != "ыудусе" {
		t.Errorf("with autoKeymap off, input must be untouched; got %q", m3.line())
	}
}

// paste отправляет всю строку одним событием KeyRunes, как терминал при
// bracketed paste (в отличие от typeStr, шлющего по одному руну).
func paste(m *editorModel, s string) {
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

func TestEditorAutoKeymapPasteBatch(t *testing.T) {
	// Вставка через границу кавычки сохраняет кириллическое значение внутри
	// кавычек как есть — проверка состояния по каждому руну, а не разом.
	m := newTestEditor()
	m.autoKeymap = true
	paste(m, "name = 'Иван'")
	if got := m.line(); got != "name = 'Иван'" {
		t.Errorf("pasted Cyrillic value inside quotes must survive; got %q", got)
	}

	// То же для идентификатора в двойных кавычках, вставленного разом.
	m2 := newTestEditor()
	m2.autoKeymap = true
	paste(m2, `from "Таблица"`)
	if got := m2.line(); got != `from "Таблица"` {
		t.Errorf("pasted Cyrillic identifier must survive; got %q", got)
	}

	// Кириллическое слово ВНЕ кавычек конвертируется полностью, даже если содержит
	// клавишу, дающую символ кавычки ('э' → '\''): 'этаж' → "'nf;" (конвертированная
	// кавычка не открывает строку).
	m3 := newTestEditor()
	m3.autoKeymap = true
	paste(m3, "этаж")
	if got := m3.line(); got != ConvertLayout("этаж") || strings.ContainsRune(got, 'э') {
		t.Errorf("Cyrillic word outside quotes must fully convert; got %q (want %q)", got, ConvertLayout("этаж"))
	}

	// Вставка, открывающая И закрывающая строку, конвертирует код вокруг неё.
	m4 := newTestEditor()
	m4.autoKeymap = true
	paste(m4, "ыудусе 'Имя' ") // "select 'Имя' "
	if got := m4.line(); got != "select 'Имя' " {
		t.Errorf("code converts, quoted value preserved; got %q", got)
	}
}
