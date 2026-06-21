package repl

import "fmt"

// Преобразование по позиции клавиш кириллица→латиница (ЙЦУКЕН → QWERTY), как Punto
// Switcher. Весь ввод в terox — SQL и мета-команды — на латинице, поэтому
// кириллический символ почти всегда означает забытую русскую раскладку. Заменяем
// на латинскую букву на ТОЙ ЖЕ физической клавише: "ыудусе" → "select", "\в" →
// "\d". Внутри строковых литералов и комментариев преобразование пропускается
// (см. editor), так что реальное кириллическое значение ('Иван') сохраняется.

// cyr2lat сопоставляет каждую русскую клавишу с латинским символом на той же клавише.
var cyr2lat = map[rune]rune{
	// верхний ряд букв
	'й': 'q', 'ц': 'w', 'у': 'e', 'к': 'r', 'е': 't', 'н': 'y', 'г': 'u',
	'ш': 'i', 'щ': 'o', 'з': 'p', 'х': '[', 'ъ': ']',
	// средний ряд
	'ф': 'a', 'ы': 's', 'в': 'd', 'а': 'f', 'п': 'g', 'р': 'h', 'о': 'j',
	'л': 'k', 'д': 'l', 'ж': ';', 'э': '\'',
	// нижний ряд
	'я': 'z', 'ч': 'x', 'с': 'c', 'м': 'v', 'и': 'b', 'т': 'n', 'ь': 'm',
	'б': ',', 'ю': '.', 'ё': '`',
	// ряды букв в верхнем регистре
	'Й': 'Q', 'Ц': 'W', 'У': 'E', 'К': 'R', 'Е': 'T', 'Н': 'Y', 'Г': 'U',
	'Ш': 'I', 'Щ': 'O', 'З': 'P', 'Х': '{', 'Ъ': '}',
	'Ф': 'A', 'Ы': 'S', 'В': 'D', 'А': 'F', 'П': 'G', 'Р': 'H', 'О': 'J',
	'Л': 'K', 'Д': 'L', 'Ж': ':', 'Э': '"',
	'Я': 'Z', 'Ч': 'X', 'С': 'C', 'М': 'V', 'И': 'B', 'Т': 'N', 'Ь': 'M',
	'Б': '<', 'Ю': '>', 'Ё': '~',
}

// convertLayoutRune возвращает латинский эквивалент кириллической клавиши или
// сам символ, если он не кириллический.
func convertLayoutRune(r rune) rune {
	if t, ok := cyr2lat[r]; ok {
		return t
	}
	return r
}

// ConvertLayout преобразует каждый кириллический символ в s в латинский эквивалент
// по позиции клавиши (ASCII проходит без изменений).
func ConvertLayout(s string) string {
	out := []rune(s)
	changed := false
	for i, r := range out {
		if t := convertLayoutRune(r); t != r {
			out[i] = t
			changed = true
		}
	}
	if !changed {
		return s
	}
	return string(out)
}

// doLayout переключает авто-преобразование кириллица→латиница и сохраняет в конфиг.
func (r *REPL) doLayout(args []string) error {
	cur := r.cfg.AutoKeymapEnabled()
	on, err := parseOnOff(args, cur)
	if err != nil {
		return fmt.Errorf("\\layout: %w", err)
	}
	r.cfg.AutoKeymap = &on
	note := ""
	// Автораскладка работает только в редакторе tea; в readline она не действует,
	// поэтому сообщаем об этом, а не молча игнорируем.
	if on && !r.useTeaEditor {
		note = " — note: takes effect only in the tea editor (current: readline; set editor: tea or TEROX_EDITOR=tea)"
	}
	if err := r.cfg.Save(); err != nil {
		fmt.Fprintf(r.out, "auto_keymap %s for this session (could not save: %v)%s\n", onOff(on), err, note)
		return nil
	}
	fmt.Fprintf(r.out, "auto_keymap (Cyrillic→Latin) %s (saved)%s\n", onOff(on), note)
	return nil
}
