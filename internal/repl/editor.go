package repl

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"terox/internal/complete"
	"terox/internal/sqlsplit"
	"terox/internal/ui"
)

// Редактор ввода на bubbletea с живым выпадающим списком автодополнения
// (обновляется на каждое нажатие): подсветка SQL, инлайн-подсказка (ghost) и
// МНОГОСТРОЧНОЕ редактирование. Enter вставляет перенос, пока оператор не завершён
// (нет завершающей ";" и это не мета-команда), и выполняет его при завершении; ↑/↓
// ходят по строкам буфера, а на краях — по истории. Это редактор по умолчанию;
// классический readline включают через TEROX_EDITOR=readline, ключ editor: в
// конфиге или команду \editor.

// errEditorInterrupt — аналог readline.ErrInterrupt для Ctrl-C.
var errEditorInterrupt = errors.New("interrupt")

// errEditorEOF — аналог io.EOF для Ctrl-D на пустой строке.
var errEditorEOF = errors.New("eof")

// menuMaxRows ограничивает число строк в выпадающем списке.
const menuMaxRows = 8

// editorModel — bubbletea-модель одной строки ввода.
type editorModel struct {
	r          *REPL
	comp       *completer
	prompt     string // уже отрендеренное (цветное) приглашение первой строки
	contPrompt string // приглашение строк-продолжений (выровнено под первой)
	autoKeymap bool   // конвертировать кириллицу в латинские позиции клавиш

	input  []rune // ввод; может содержать '\n' (многострочный оператор)
	cursor int    // индекс руны в input

	hist    []string
	histIdx int    // == len(hist) означает "текущую (несохранённую) строку"
	stash   string // строка в работе, сохранённая на время просмотра истории

	// Обратный поиск по истории (Ctrl-R).
	searching   bool
	searchQuery []rune
	searchIdx   int    // индекс текущего совпадения в hist (-1 = нет)
	preSearch   []rune // ввод для восстановления при отмене поиска
	preCursor   int

	// состояние выпадающего списка автодополнения
	cands      []string // отображаемые формы (всё слово: prefix+suffix)
	full       []string // текст для ВСТАВКИ каждого кандидата (только suffix)
	detail     []string // тип/сигнатура кандидата для тусклой пометки
	sel        int
	menuOpen   bool
	menuActive bool // пользователь вошёл в меню стрелками → Enter принимает
	dismissed  bool // Esc закрывает меню до следующего редактирования
	forceMenu  bool // Tab принудительно открывает меню даже после пробела

	width int

	// итог сеанса
	done        bool
	submitted   string
	interrupted bool
	eof         bool
}

func (m *editorModel) Init() tea.Cmd { return nil }

// line возвращает текущий ввод как строку.
func (m *editorModel) line() string { return string(m.input) }

// recompute обновляет кандидатов автодополнения для текущего ввода.
// suggestions() возвращает суффиксы для добавления и длину уже набранного
// префикса в рунах; список показывает всё слово (prefix+suffix) и запоминает
// суффикс для вставки при выборе.
func (m *editorModel) recompute() {
	m.cands = m.cands[:0]
	m.full = m.full[:0]
	m.detail = m.detail[:0]
	force := m.forceMenu
	m.forceMenu = false
	m.menuActive = false // редактирование текста выходит из режима навигации по меню
	if m.dismissed {
		m.menuOpen = false
		return
	}
	// Что дополнять берётся из текста ЛЕВЕЕ КУРСОРА, но область видимости
	// (алиас→отношение) — из ВСЕЙ строки, поэтому колонки дополняются после алиаса,
	// даже если его FROM правее курсора ("select i.| from t i").
	//
	// m.cursor — индекс в РУНАХ, а suggestions/sqlResult и движок дополнения режут по
	// БАЙТОВОМУ смещению, поэтому переводим в байтовую длину head: иначе многобайтовый
	// префикс (кириллица и т.п.) анализировал бы не то слово.
	line := string(m.input)
	head := string(m.input[:m.cursor])
	pos := len(head)
	exact := false
	trimmed := strings.TrimLeft(head, " \t")
	if strings.HasPrefix(trimmed, "\\") {
		subs, n := m.comp.suggestions(line, pos)
		hr := []rune(head)
		prefix := ""
		if n >= 0 && n <= len(hr) {
			prefix = string(hr[len(hr)-n:])
		}
		for _, suf := range subs {
			m.cands = append(m.cands, prefix+suf)
			m.full = append(m.full, suf)
			m.detail = append(m.detail, "")
		}
	} else {
		// Меню строится из тех же ранжированных кандидатов, что и ghost, поэтому ghost,
		// выделенная строка и Tab всегда совпадают. При явном Tab (force) недостающие
		// колонки загружаются СИНХРОННО, чтобы меню показало их сразу.
		res := m.comp.sqlResult(line, pos, force)
		prefix := head[res.ReplaceStart:]
		filterCandidates(res.Candidates, prefix, func(cand complete.Candidate, suf string) bool {
			if suf == "" {
				exact = true // полное имя уже набрано
				return true
			}
			m.cands = append(m.cands, prefix+suf)
			m.full = append(m.full, suf)
			m.detail = append(m.detail, candDetail(cand))
			return true
		})
	}
	if m.sel >= len(m.cands) {
		m.sel = 0
	}
	// Авто-открытие только при наборе токена (autoTrigger). После пробела/запятой/";"
	// меню закрыто до принудительного Tab. Если набранное уже точно совпадает с именем,
	// длинные альтернативы автоматически не показываются.
	m.menuOpen = len(m.cands) > 0 && !exact && (force || autoTrigger(head))
}

// candDetail формирует тусклую пометку строки автодополнения: тип колонки,
// сигнатуру функции или вид+схему отношения.
func candDetail(c complete.Candidate) string {
	if c.Detail == "" {
		return ""
	}
	if c.Kind == complete.KColumn {
		return ":" + c.Detail
	}
	return c.Detail
}

// acceptSelected вставляет выбранное автодополнение (его суффикс) у курсора.
func (m *editorModel) acceptSelected() {
	if !m.menuOpen || m.sel >= len(m.full) {
		return
	}
	m.insert([]rune(m.full[m.sel]))
}

// maybeConvertLayout конвертирует руны кириллицы в латинский эквивалент по клавишам
// (autoKeymap), КРОМЕ случая, когда курсор внутри строкового литерала или
// комментария — там реальное кириллическое значение (например 'Иван') сохраняется как есть.
func (m *editorModel) maybeConvertLayout(rs []rune) []rune {
	if !m.autoKeymap {
		return rs
	}
	// Кириллица сохраняется как есть внутри ЛЮБЫХ кавычек ('...', "...", $tag$...$, E'...')
	// и комментариев, чтобы реальные значения/идентификаторы на русском не ломались.
	// Состояние лексера пересчитывается ПОРУННО: вставленный фрагмент может пересекать
	// границу кавычки (например "name = 'Иван'" приходит одним событием KeyRunes), а
	// единая проверка заранее залатинизировала бы значение внутри кавычек.
	//
	// Состояние отслеживается по СЫРОМУ вводу пользователя: строку ограничивают ASCII-кавычки,
	// которые он действительно набрал. (Отслеживание по сконвертированному выводу позволило бы
	// кириллической клавише, отображаемой в кавычку — 'э'→'\'' — ложно открыть строку и
	// сломать autoKeymap обычного слова вроде "этаж".)
	prefix := append([]rune(nil), m.input[:m.cursor]...)
	out := make([]rune, len(rs))
	for i, r := range rs {
		switch complete.TrailingStateOf(string(prefix)) {
		case complete.StateString, complete.StateComment, complete.StateQIdent:
			out[i] = r
		default:
			out[i] = convertLayoutRune(r)
		}
		prefix = append(prefix, r)
	}
	return out
}

func (m *editorModel) insert(rs []rune) {
	tail := append([]rune{}, m.input[m.cursor:]...)
	m.input = append(m.input[:m.cursor], append(rs, tail...)...)
	m.cursor += len(rs)
	m.dismissed = false
	m.recompute()
}

func (m *editorModel) backspace() {
	if m.cursor == 0 {
		return
	}
	m.input = append(m.input[:m.cursor-1], m.input[m.cursor:]...)
	m.cursor--
	m.dismissed = false
	m.recompute()
}

// wordLeft возвращает индекс курсора на одно слово левее (пропуск пробельных, затем
// слово). wordRight — зеркальный. Границей слова считается любой пробельный символ
// (пробел/таб/перевод строки), иначе word-motion в многострочном вводе перепрыгивал
// бы через '\n'/'\t', сцепляя соседние строки.
func (m *editorModel) wordLeft() int {
	i := m.cursor
	for i > 0 && unicode.IsSpace(m.input[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(m.input[i-1]) {
		i--
	}
	return i
}

func (m *editorModel) wordRight() int {
	i, n := m.cursor, len(m.input)
	for i < n && unicode.IsSpace(m.input[i]) {
		i++
	}
	for i < n && !unicode.IsSpace(m.input[i]) {
		i++
	}
	return i
}

func (m *editorModel) deleteWord() {
	if m.cursor == 0 {
		return
	}
	i := m.cursor
	for i > 0 && unicode.IsSpace(m.input[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(m.input[i-1]) {
		i--
	}
	m.input = append(m.input[:i], m.input[m.cursor:]...)
	m.cursor = i
	m.dismissed = false
	m.recompute()
}

// inputComplete сообщает, готов ли ввод к выполнению (а не к переносу строки):
// пусто, мета-команда (\...) или masked-хвост оканчивается на ";". Маскируем
// литералы/комментарии, чтобы ";" внутри строки не считалась завершением.
func (m *editorModel) inputComplete() bool {
	trimmed := strings.TrimSpace(m.line())
	if trimmed == "" || strings.HasPrefix(trimmed, "\\") {
		return true
	}
	return strings.HasSuffix(strings.TrimSpace(sqlsplit.Mask(m.line())), ";")
}

// insertNewline вставляет перенос строки в позиции курсора и закрывает меню (на
// свежей строке автодополнение не всплывает, пока не начат набор).
func (m *editorModel) insertNewline() {
	tail := append([]rune{}, m.input[m.cursor:]...)
	m.input = append(m.input[:m.cursor], append([]rune{'\n'}, tail...)...)
	m.cursor++
	m.menuOpen, m.menuActive = false, false
	m.cands, m.full, m.detail = m.cands[:0], m.full[:0], m.detail[:0]
}

// lineBounds возвращает индексы начала и конца (по руне) логической строки, на
// которой стоит pos: start — сразу за предыдущим '\n' (или 0), end — на следующем
// '\n' (или len).
func (m *editorModel) lineBounds(pos int) (start, end int) {
	start = 0
	for i := pos - 1; i >= 0; i-- {
		if m.input[i] == '\n' {
			start = i + 1
			break
		}
	}
	end = len(m.input)
	for i := pos; i < len(m.input); i++ {
		if m.input[i] == '\n' {
			end = i
			break
		}
	}
	return start, end
}

// moveCursorVert двигает курсор на строку вверх (dir=-1) или вниз (dir=+1) внутри
// многострочного буфера, сохраняя визуальную колонку. Возвращает false, если двигаться
// в этом направлении некуда (курсор на крайней строке) — тогда вызывающий уходит в
// историю.
func (m *editorModel) moveCursorVert(dir int) bool {
	start, end := m.lineBounds(m.cursor)
	col := m.cursor - start
	if dir < 0 {
		if start == 0 {
			return false // первая строка
		}
		prevStart, prevEnd := m.lineBounds(start - 1)
		m.cursor = prevStart + col
		if m.cursor > prevEnd {
			m.cursor = prevEnd
		}
	} else {
		if end >= len(m.input) {
			return false // последняя строка
		}
		nextStart, nextEnd := m.lineBounds(end + 1)
		m.cursor = nextStart + col
		if m.cursor > nextEnd {
			m.cursor = nextEnd
		}
	}
	m.menuActive = false
	return true
}

// cursorRowCol возвращает (строка, колонка) курсора в рунах для отрисовки.
func (m *editorModel) cursorRowCol() (row, col int) {
	for i := 0; i < m.cursor && i < len(m.input); i++ {
		if m.input[i] == '\n' {
			row++
			col = 0
		} else {
			col++
		}
	}
	return row, col
}

func (m *editorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyMsg:
		if m.searching {
			return m.updateSearch(msg)
		}
		switch msg.Type {
		case tea.KeyCtrlR:
			m.searching = true
			m.searchQuery = nil
			m.searchIdx = -1
			m.preSearch = append([]rune(nil), m.input...)
			m.preCursor = m.cursor
			m.menuOpen = false
			return m, nil
		case tea.KeyCtrlC:
			m.interrupted = true
			m.done = true
			return m, tea.Quit
		case tea.KeyCtrlD:
			if len(m.input) == 0 {
				m.eof = true
				m.done = true
				return m, tea.Quit
			}
			// Ctrl-D в середине строки удаляет символ под курсором (как в readline).
			if m.cursor < len(m.input) {
				m.input = append(m.input[:m.cursor], m.input[m.cursor+1:]...)
				m.dismissed = false
				m.recompute()
			}
			return m, nil
		case tea.KeyCtrlL:
			// Очистить экран и перерисовать приглашение с текущим вводом, как Ctrl-L
			// в readline.
			return m, tea.ClearScreen
		case tea.KeyEnter:
			// Если пользователь вошёл в меню стрелками, Enter принимает выделенный
			// элемент, а не выполняет ввод.
			if m.menuOpen && m.menuActive {
				m.acceptSelected()
				m.menuActive = false
				return m, nil
			}
			// Многострочность: выполняем, только когда оператор завершён — пустой ввод,
			// мета-команда (\...) или masked-хвост оканчивается на ";". Иначе Enter
			// добавляет перенос и продолжает редактирование (как accumulation в psql,
			// но прямо в редакторе — со свободной навигацией по строкам).
			if m.inputComplete() {
				m.submitted = m.line()
				m.done = true
				m.menuOpen = false
				return m, tea.Quit
			}
			m.insertNewline()
			return m, nil
		case tea.KeyTab:
			if m.menuOpen {
				m.acceptSelected()
			} else {
				// Явное автодополнение: принудительно открыть меню даже после пробела.
				m.dismissed = false
				m.forceMenu = true
				m.recompute()
			}
			return m, nil
		case tea.KeyEsc:
			if m.menuOpen {
				m.menuOpen = false
				m.menuActive = false
				m.dismissed = true
			}
			return m, nil
		case tea.KeyUp:
			if m.menuOpen {
				m.sel = (m.sel - 1 + len(m.cands)) % len(m.cands)
				m.menuActive = true
			} else if !m.moveCursorVert(-1) {
				// Уже на первой строке буфера — поднимаемся в историю.
				m.historyPrev()
			} else {
				m.recompute()
			}
			return m, nil
		case tea.KeyDown:
			if m.menuOpen {
				m.sel = (m.sel + 1) % len(m.cands)
				m.menuActive = true
			} else if !m.moveCursorVert(1) {
				// Уже на последней строке буфера — спускаемся в историю.
				m.historyNext()
			} else {
				m.recompute()
			}
			return m, nil
		case tea.KeyRight:
			m.menuActive = false
			if m.cursor >= len(m.input) {
				m.acceptGhost()
				return m, nil
			}
			if msg.Alt {
				m.cursor = m.wordRight()
			} else {
				m.cursor++
			}
			m.recompute() // автодополнение следует за курсором
			return m, nil
		case tea.KeyLeft:
			m.menuActive = false
			if msg.Alt {
				m.cursor = m.wordLeft()
			} else if m.cursor > 0 {
				m.cursor--
			}
			m.recompute()
			return m, nil
		case tea.KeyCtrlRight:
			m.menuActive = false
			m.cursor = m.wordRight()
			m.recompute()
			return m, nil
		case tea.KeyCtrlLeft:
			m.menuActive = false
			m.cursor = m.wordLeft()
			m.recompute()
			return m, nil
		case tea.KeyHome, tea.KeyCtrlA:
			m.menuActive = false
			m.cursor, _ = m.lineBounds(m.cursor) // к началу текущей строки
			m.recompute()
			return m, nil
		case tea.KeyEnd, tea.KeyCtrlE:
			m.menuActive = false
			_, m.cursor = m.lineBounds(m.cursor) // к концу текущей строки
			m.recompute()
			return m, nil
		case tea.KeyCtrlU:
			// Удалить от начала текущей строки до курсора.
			start, _ := m.lineBounds(m.cursor)
			m.input = append(m.input[:start], m.input[m.cursor:]...)
			m.cursor = start
			m.dismissed = false
			m.recompute()
			return m, nil
		case tea.KeyCtrlK:
			// Удалить от курсора до конца текущей строки.
			_, end := m.lineBounds(m.cursor)
			m.input = append(m.input[:m.cursor], m.input[end:]...)
			m.dismissed = false
			m.recompute()
			return m, nil
		case tea.KeyCtrlW:
			m.deleteWord()
			return m, nil
		case tea.KeyBackspace:
			m.backspace()
			return m, nil
		case tea.KeyDelete:
			if m.cursor < len(m.input) {
				m.input = append(m.input[:m.cursor], m.input[m.cursor+1:]...)
				m.dismissed = false
				m.recompute()
			}
			return m, nil
		case tea.KeySpace:
			m.insert([]rune{' '})
			return m, nil
		case tea.KeyRunes:
			m.insert(m.maybeConvertLayout(msg.Runes))
			return m, nil
		}
	}
	return m, nil
}

// acceptGhost (стрелка вправо в конце строки) принимает видимый ghost, то есть
// выделенную строку меню — как Tab, чтобы все три варианта совпадали.
// updateSearch обрабатывает клавиши в режиме обратного поиска по истории (Ctrl-R).
func (m *editorModel) updateSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlR: // следующее (более старое) совпадение
		m.searchStep(m.searchIdx - 1)
	case tea.KeyCtrlC, tea.KeyEsc, tea.KeyCtrlG: // отмена — восстановить исходный ввод
		m.input = m.preSearch
		m.cursor = m.preCursor
		m.exitSearch()
	case tea.KeyEnter: // принять совпадение в строку (пока не выполнять)
		m.exitSearch()
		m.cursor = len(m.input)
	case tea.KeyBackspace:
		if len(m.searchQuery) > 0 {
			m.searchQuery = m.searchQuery[:len(m.searchQuery)-1]
			m.searchStep(len(m.hist) - 1)
		}
	case tea.KeySpace:
		m.searchQuery = append(m.searchQuery, ' ')
		m.searchStep(len(m.hist) - 1)
	case tea.KeyRunes:
		m.searchQuery = append(m.searchQuery, msg.Runes...)
		m.searchStep(len(m.hist) - 1)
	default:
		// Любая другая клавиша (стрелки, Tab, …) выходит из поиска, сохраняя совпадение,
		// и затем применяется обычным образом.
		m.exitSearch()
		m.cursor = len(m.input)
		return m.Update(msg)
	}
	return m, nil
}

// searchStep ищет самую свежую запись истории на позиции `from` или раньше,
// содержащую запрос, и помещает её в строку ввода.
func (m *editorModel) searchStep(from int) {
	q := strings.ToLower(string(m.searchQuery))
	for i := from; i >= 0; i-- {
		if i < len(m.hist) && strings.Contains(strings.ToLower(m.hist[i]), q) {
			m.searchIdx = i
			m.input = []rune(m.hist[i])
			m.cursor = len(m.input)
			return
		}
	}
	// нет совпадения: запрос показывается дальше, строка остаётся последним совпадением (или пустой)
}

func (m *editorModel) exitSearch() {
	m.searching = false
	m.searchQuery = nil
	m.searchIdx = -1
}

func (m *editorModel) acceptGhost() {
	if m.menuOpen && m.sel < len(m.full) {
		m.insert([]rune(m.full[m.sel]))
	}
}

func (m *editorModel) historyPrev() {
	if len(m.hist) == 0 || m.histIdx == 0 {
		return
	}
	if m.histIdx == len(m.hist) {
		m.stash = m.line()
	}
	m.histIdx--
	m.input = []rune(m.hist[m.histIdx])
	m.cursor = len(m.input)
	m.dismissed = true
	m.menuOpen = false
}

func (m *editorModel) historyNext() {
	if m.histIdx >= len(m.hist) {
		return
	}
	m.histIdx++
	if m.histIdx == len(m.hist) {
		m.input = []rune(m.stash)
	} else {
		m.input = []rune(m.hist[m.histIdx])
	}
	m.cursor = len(m.input)
	m.dismissed = true
	m.menuOpen = false
}

func (m *editorModel) View() string {
	if m.done {
		// Оставить на экране только итоговый ввод (со всеми строками, без списка).
		return m.renderInput(false) + "\n"
	}
	if m.searching {
		// приглашение reverse-i-search вместо обычного.
		status := ""
		if len(m.hist) > 0 && m.searchIdx < 0 && len(m.searchQuery) > 0 {
			status = ui.Danger.Render("  (no match)")
		}
		return ui.Dim.Render("(reverse-i-search)`") + string(m.searchQuery) +
			ui.Dim.Render("': ") + highlightSQL(m.line()) + status
	}

	var b strings.Builder
	// Многострочный ввод с приглашениями строк и видимым блоком курсора.
	b.WriteString(m.renderInput(true))
	// Инлайн-ghost (тусклый) = продолжение ВЫДЕЛЕННОЙ строки меню, поэтому ghost,
	// выделенная строка и вставляемое по Tab всегда совпадают. Только когда меню
	// открыто и курсор в конце строки.
	if m.r.suggest && m.menuOpen && m.cursor == len(m.input) && m.sel < len(m.full) {
		ghost := m.full[m.sel]
		annot := m.detail[m.sel]
		if annot != "" {
			annot = "  " + annot
		}
		if more := len(m.cands) - 1; more > 0 {
			annot += "  +" + itoa(more)
		}
		b.WriteString(ui.Dim.Render(ghost + annot))
	}
	if m.menuOpen {
		b.WriteString("\n")
		b.WriteString(m.renderMenu())
	}
	return b.String()
}

// renderInput рисует весь (возможно многострочный) ввод: первую строку с основным
// приглашением, продолжения — с выровненным contPrompt, с подсветкой SQL. При
// withCursor на позиции курсора рисуется блочный курсор (символ под ним инверсией,
// либо завершающий пробел в конце строки), чтобы курсор был всегда виден.
func (m *editorModel) renderInput(withCursor bool) string {
	cur := lipgloss.NewStyle().Reverse(true)
	hl := func(s string) string {
		if !ui.Enabled {
			return s
		}
		return highlightSQL(s)
	}
	row, col := -1, -1
	if withCursor {
		row, col = m.cursorRowCol()
	}
	lines := strings.Split(string(m.input), "\n")
	var b strings.Builder
	for i, ln := range lines {
		if i == 0 {
			b.WriteString(m.prompt)
		} else {
			b.WriteString("\n")
			b.WriteString(m.contPrompt)
		}
		if i != row {
			b.WriteString(hl(ln))
			continue
		}
		runes := []rune(ln)
		if col >= len(runes) {
			b.WriteString(hl(ln) + cur.Render(" "))
		} else {
			b.WriteString(hl(string(runes[:col])) + cur.Render(string(runes[col])) + hl(string(runes[col+1:])))
		}
	}
	return b.String()
}

// renderMenu рисует выпадающий список в рамке, подсвечивая выбранную строку.
func (m *editorModel) renderMenu() string {
	start := 0
	if m.sel >= menuMaxRows {
		start = m.sel - menuMaxRows + 1
	}
	end := start + menuMaxRows
	if end > len(m.cands) {
		end = len(m.cands)
	}
	selStyle := lipgloss.NewStyle().Reverse(true)
	// Дополнить имена до общей ширины, чтобы колонка тусклого типа/детали выровнялась.
	nameW := 0
	for i := start; i < end; i++ {
		if w := len([]rune(m.cands[i])); w > nameW {
			nameW = w
		}
	}
	var rows []string
	for i := start; i < end; i++ {
		name := m.cands[i]
		if pad := nameW - len([]rune(name)); pad > 0 {
			name += strings.Repeat(" ", pad)
		}
		name = "  " + name
		if i == m.sel {
			name = selStyle.Render(name)
		}
		row := name
		if d := m.detail[i]; d != "" {
			row += "  " + ui.Dim.Render(d)
		}
		rows = append(rows, row)
	}
	if len(m.cands) > end {
		rows = append(rows, ui.Dim.Render("  … +"+itoa(len(m.cands)-end)+" more"))
	}
	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240"))
	return box.Render(strings.Join(rows, "\n"))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// doEditor переключает интерактивный редактор строки в рантайме и запоминает выбор
// в конфиге. Без аргумента открывает выбор; "\editor tea" или "\editor readline"
// задают его напрямую.
func (r *REPL) doEditor(args []string) error {
	choice := ""
	if len(args) > 0 {
		choice = strings.ToLower(strings.TrimSpace(args[0]))
	} else {
		c, aborted, err := huhSelect("Line editor", []string{
			"readline (classic: highlight, ghost, Ctrl-R)",
			"tea (live completion dropdown)",
		})
		if err != nil || aborted {
			return err
		}
		if strings.HasPrefix(c, "tea") {
			choice = "tea"
		} else {
			choice = "readline"
		}
	}

	switch choice {
	case "tea":
		r.useTeaEditor = true
		// Перечитываем историю с диска при каждом переключении в tea: в режиме
		// readline завершённые операторы попадают только в файл (recordHistory не
		// пишет in-memory вне tea), поэтому без перечитывания они бы не появились в
		// навигации tea до перезапуска. Диск — источник истины (recordHistory всегда
		// сохраняет туда не-секретные завершённые операторы).
		r.history = loadHistoryLines(r.histPath)
	case "readline", "classic":
		r.useTeaEditor = false
	default:
		return fmt.Errorf("unknown editor %q (use 'tea' or 'readline')", choice)
	}

	// Сохранить, чтобы следующий запуск использовал тот же редактор.
	stored := "readline"
	if r.useTeaEditor {
		stored = "tea"
	}
	r.cfg.Editor = stored
	if err := r.cfg.Save(); err != nil {
		fmt.Fprintf(r.out, "editor set to %s for this session (could not save: %v)\n", stored, err)
		return nil
	}
	fmt.Fprintf(r.out, "editor: %s (saved; effective on the next input line)\n", stored)
	return nil
}

// readLineTea запускает bubbletea-редактор для одной строки и возвращает текст.
func (r *REPL) readLineTea(prompt string) (string, error) {
	m := &editorModel{
		r:          r,
		comp:       r.comp,
		prompt:     prompt,
		contPrompt: r.prompt(true),
		hist:       r.history,
		histIdx:    len(r.history),
		autoKeymap: r.cfg.AutoKeymapEnabled(),
	}
	out, err := tea.NewProgram(m, tea.WithOutput(r.out)).Run()
	if err != nil {
		return "", err
	}
	fm := out.(*editorModel)
	switch {
	case fm.interrupted:
		return "", errEditorInterrupt
	case fm.eof:
		return "", errEditorEOF
	}
	return fm.submitted, nil
}
