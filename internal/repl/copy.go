package repl

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"terox/internal/execution"
	"terox/internal/migration"
	"terox/internal/sqlsplit"
)

// Feature 15: клиентский COPY. \copy <src> to|from <file> [csv|text|tsv].
// Всё клиентское: terox сам открывает локальный файл, сервер видит только
// STDIN/STDOUT — серверный COPY ... TO/FROM '<path>' и TO PROGRAM невозможны.
// Источник строго валидируется, чтобы исключить инъекцию (например "t TO PROGRAM …").

// copyTableRe — допустимый табличный источник: [schema.]table с необязательным
// списком простых колонок. Без пробелов/ключевых слов вне списка — поэтому в
// COPY <src> ... нельзя подсунуть "TO PROGRAM"/";".
var copyTableRe = regexp.MustCompile(`^[A-Za-z_][\w$]*(\.[A-Za-z_][\w$]*)?(\s*\(\s*[A-Za-z_][\w$]*(\s*,\s*[A-Za-z_][\w$]*)*\s*\))?$`)

// copyFormatOption переводит csv|text|tsv в WITH-опции COPY.
func copyFormatOption(format string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "csv":
		return "(FORMAT csv, HEADER true)", nil
	case "text", "tsv":
		return "(FORMAT text)", nil
	default:
		return "", fmt.Errorf("unknown copy format %q (want csv, text, or tsv)", format)
	}
}

// findCopyDirection ищет ключевое слово to/from как отдельное слово на глубине
// скобок 0, возвращая (индекс, направление). Так `(select …) to f` и `t(a,b) from f`
// разбираются корректно, а to/from внутри скобок источника игнорируется.
func findCopyDirection(s string) (int, string) {
	// Сканируем по МАСКИРОВАННОЙ строке (литералы/комментарии обнулены, но длина и
	// смещения сохранены), чтобы скобки и слова to/from внутри строковых литералов
	// не учитывались — иначе валидатор расходится с серверным лексером.
	m := sqlsplit.Mask(s)
	low := strings.ToLower(m)
	depth := 0
	for i := 0; i < len(m); i++ {
		switch m[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
		if depth != 0 {
			continue
		}
		for _, kw := range []string{"to", "from"} {
			n := len(kw)
			// Разделитель — любой пробельный символ (включая '\n'/'\r' для многострочного
			// `\copy t\nfrom file`) или ')' слева. Сужать до пробела/таба нельзя:
			// многострочный ввод иначе не парсится.
			if i+n <= len(low) && low[i:i+n] == kw &&
				(i == 0 || isCopySep(m[i-1]) || m[i-1] == ')') &&
				(i+n == len(m) || isCopySep(m[i+n])) {
				return i, kw
			}
		}
	}
	return -1, ""
}

// isCopySep сообщает, является ли байт пробельным разделителем токенов в \copy
// (пробел/таб/перевод строки/возврат каретки) — чтобы to/from распознавались и в
// многострочном вводе.
func isCopySep(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// validateCopySource проверяет источник: либо (запрос) со сбалансированными
// скобками и закрывающей скобкой в самом конце, либо простой [schema.]table[(cols)].
// Для запроса дополнительно требуется read-only (экспорт не должен писать).
func validateCopySource(src string, allowQuery bool) error {
	src = strings.TrimSpace(src)
	if src == "" {
		return fmt.Errorf("empty COPY source")
	}
	if strings.HasPrefix(src, "(") {
		if !allowQuery {
			return fmt.Errorf("COPY FROM requires a table, not a query")
		}
		// Баланс/трейлинг считаем по маске (скобки внутри литералов не в счёт),
		// чтобы валидатор совпадал с серверным лексером (см. findCopyDirection).
		m := sqlsplit.Mask(src)
		depth := 0
		for i := 0; i < len(m); i++ {
			switch m[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 && i != len(m)-1 {
					return fmt.Errorf("COPY source query must end at its closing ')' (no trailing text)")
				}
			}
		}
		if depth != 0 {
			return fmt.Errorf("COPY source query has unbalanced parentheses")
		}
		inner := strings.TrimSpace(src[1 : len(src)-1])
		if execution.IsWrite(inner) {
			return fmt.Errorf("COPY (query) TO is read-only; the query must not write")
		}
		return nil
	}
	if !copyTableRe.MatchString(src) {
		return fmt.Errorf("invalid COPY table source %q (use [schema.]table[(col,...)] or a (SELECT ...) query)", src)
	}
	return nil
}

// atomicWriteFile записывает результат через ВРЕМЕННЫЙ файл рядом с целевым и
// делает atomic rename в место назначения только при успешном run (R-NEW-4).
// Если run вернул ошибку (например, сервер отверг COPY), временный файл
// удаляется, а существующий файл назначения остаётся НЕтронутым — частично
// записанный/пустой результат не затирает прежний. run получает Writer временного
// файла и возвращает число записанных строк.
func atomicWriteFile(file string, run func(io.Writer) (int64, error)) (int64, error) {
	dir := filepath.Dir(file)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(file)+".terox-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	n, runErr := run(tmp)
	if runErr != nil {
		tmp.Close()
		os.Remove(tmpName)
		return n, runErr
	}
	// fsync перед rename, чтобы после atomic rename содержимое гарантированно было
	// на диске (а не только в page cache) — иначе при сбое можно получить пустой файл.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return n, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return n, err
	}
	if err := os.Rename(tmpName, file); err != nil {
		os.Remove(tmpName)
		return n, err
	}
	return n, nil
}

// parseCopyTail разбирает хвост `\copy ... to|from <tail>`: первый токен — путь к
// файлу, далее в любом порядке необязательные format (csv|text|tsv) и/или force
// (для TO — перезапись существующего файла без подтверждения). Дублирующийся
// format/неизвестный лишний токен — ошибка, чтобы опечатка не была проглочена.
func parseCopyTail(toks []string) (file, format string, force bool, err error) {
	if len(toks) == 0 {
		return "", "", false, fmt.Errorf("\\copy: missing file path")
	}
	file = toks[0]
	for _, tok := range toks[1:] {
		if strings.EqualFold(tok, "force") {
			force = true
			continue
		}
		if format != "" {
			return "", "", false, fmt.Errorf("\\copy: unexpected token %q (want: <file> [csv|text|tsv] [force])", tok)
		}
		format = tok
	}
	return file, format, force, nil
}

// doCopy реализует \copy <src> to|from <file> [csv|text|tsv] [force].
func (r *REPL) doCopy(raw string) error {
	body := strings.TrimSpace(rawTail(raw, 1))
	idx, dir := findCopyDirection(body)
	if idx < 0 {
		return fmt.Errorf("usage: \\copy <table|(query)> to <file> [csv|text|tsv] [force]  |  \\copy <table> from <file> [csv|tsv]")
	}
	src := strings.TrimSpace(body[:idx])
	tailToks := tokenizeArgs(strings.TrimSpace(body[idx+len(dir):]))
	if len(tailToks) == 0 {
		return fmt.Errorf("\\copy: missing file path")
	}
	file, format, force, err := parseCopyTail(tailToks)
	if err != nil {
		return err
	}
	// force управляет перезаписью существующего файла и имеет смысл только для TO
	// (экспорт пишет файл). Для FROM это no-op, который легко принять за «partial
	// import / skip bad rows / bypass safety», — отклоняем явной ошибкой.
	if dir == "from" && force {
		return fmt.Errorf("force is only valid for COPY TO file overwrite")
	}
	opt, err := copyFormatOption(format)
	if err != nil {
		return err
	}
	if len(r.targets) != 1 {
		return fmt.Errorf("\\copy targets a single shard (COPY is per-backend) — narrow to one shard first; current selection has %d", len(r.targets))
	}
	target := r.targets[0]

	if dir == "to" { // экспорт (read-only на сервере)
		if err := validateCopySource(src, true); err != nil {
			return err
		}
		// Существующий файл не затираем молча: либо force, либо подтверждение.
		// Перезапись локального результата необратима, а сам atomic rename снаружи
		// неотличим от «свежей» записи.
		if !force {
			if _, statErr := os.Stat(file); statErr == nil {
				if strings.TrimSpace(r.readLine(fmt.Sprintf("%s exists — overwrite? type 'yes' (or re-run with a trailing 'force'): ", file))) != "yes" {
					fmt.Fprintln(r.out, "cancelled")
					return nil
				}
			}
		}
		n, err := atomicWriteFile(file, func(w io.Writer) (int64, error) {
			ctx, cancel := interruptible()
			defer cancel()
			cctx, c2 := contextWithOptionalTimeout(ctx, time.Duration(r.cfg.MigrationTimeout))
			defer c2()
			return r.mgr.CopyTo(cctx, target, w, fmt.Sprintf("COPY %s TO STDOUT WITH %s", src, opt))
		})
		if err != nil {
			return fmt.Errorf("copy to %s: %v (destination left unchanged)", file, err)
		}
		fmt.Fprintf(r.out, "copied %d row(s) from %s to %s\n", n, target.LabelDB(), file)
		return nil
	}

	// from: загрузка (запись) — идёт под той же защитной моделью, что и обычная
	// запись: migration role + локальные statement/lock timeouts + транзакция с
	// откатом (R-NEW-1). Guard-операторы валидируются/экранируются здесь.
	if err := validateCopySource(src, false); err != nil {
		return err
	}
	if !r.writeMode {
		return fmt.Errorf("\\copy from loads data — enable write mode first (\\write on)")
	}
	setup, err := migration.SessionGuards(r.role(), r.stmtTimeout, r.cfg.LockTimeout)
	if err != nil {
		return fmt.Errorf("\\copy from aborted: %v", err)
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(r.out, "COPY FROM %s → %s table %s (%s)\n", file, target.LabelDB(), src, strings.TrimPrefix(strings.TrimSuffix(opt, ")"), "("))
	if len(setup) > 0 {
		fmt.Fprintf(r.out, "  protected: %s\n", strings.Join(setup, "; "))
	} else {
		fmt.Fprintln(r.out, "  note: no migration role/timeout configured — loading under the connection role")
	}
	if r.writeApprove {
		if strings.TrimSpace(r.readLine("type 'yes' to load: ")) != "yes" {
			fmt.Fprintln(r.out, "cancelled")
			return nil
		}
	}
	ctx, cancel := interruptible()
	defer cancel()
	cctx, c2 := contextWithOptionalTimeout(ctx, time.Duration(r.cfg.MigrationTimeout))
	defer c2()
	n, err := r.mgr.CopyFromTx(cctx, target, f, fmt.Sprintf("COPY %s FROM STDIN WITH %s", src, opt), setup)
	if err != nil {
		return fmt.Errorf("copy from %s: %v (transaction rolled back; no rows committed)", file, err)
	}
	fmt.Fprintf(r.out, "loaded %d row(s) into %s on %s\n", n, src, target.LabelDB())
	return nil
}
