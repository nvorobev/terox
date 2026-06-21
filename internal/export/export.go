// Package export — запись результатов запроса в CSV или JSON.
package export

import (
	"bytes"
	"database/sql/driver"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// bytesValue форматирует столбец []byte. Текстовые данные (text/json/jsonb)
// возвращаются как есть; настоящая бинарщина (bytea) без валидного UTF-8 или с
// NUL кодируется в hex по образцу psql ("\xdeadbeef"), чтобы CSV оставался
// печатаемым, а JSON не портил невалидный UTF-8 в U+FFFD.
func bytesValue(b []byte) string {
	if utf8.Valid(b) && bytes.IndexByte(b, 0) < 0 {
		return string(b)
	}
	return `\x` + hex.EncodeToString(b)
}

// formatUUID форматирует 16-байтовый массив (как pgx отдаёт тип uuid) в
// каноническую строку 8-4-4-4-12, а не "[114 158 ...]".
func formatUUID(b [16]byte) string {
	const hexdig = "0123456789abcdef"
	var buf [36]byte
	j := 0
	for i := range 16 {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf[j] = '-'
			j++
		}
		buf[j] = hexdig[b[i]>>4]
		buf[j+1] = hexdig[b[i]&0x0f]
		j += 2
	}
	return string(buf[:])
}

// cell форматирует значение как строку для CSV. Текст возвращается как есть;
// только numeric-типы pgtype (через driver.Valuer) теряют хвостовые нули.
func cell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case [16]byte:
		return formatUUID(t)
	case []byte:
		return bytesValue(t)
	case time.Time:
		return t.Format(time.RFC3339Nano)
	case string:
		return t
	case float64:
		return formatFloat(t, 64)
	case float32:
		return formatFloat(float64(t), 32)
	}
	if vr, ok := v.(driver.Valuer); ok {
		if dv, err := vr.Value(); err == nil && dv != nil {
			if s, ok := dv.(string); ok {
				return trimNumeric(s) // из numeric-типа
			}
			// Рекурсия только на один уровень: Value(), возвращающий другой
			// Valuer, нарушает контракт драйвера; защита от бесконечной цепочки.
			if _, again := dv.(driver.Valuer); !again {
				return cell(dv)
			}
			return fmt.Sprintf("%v", dv)
		}
	}
	return fmt.Sprintf("%v", v)
}

// jsonValue приводит значение к виду, который json.Marshal выведет разумно:
// []byte -> string, numeric pgtype -> json.Number (без кавычек), иначе как есть.
func jsonValue(v any) any {
	switch t := v.(type) {
	case [16]byte:
		return formatUUID(t)
	case []byte:
		return bytesValue(t)
	case float64:
		return jsonFloat(t, 64)
	case float32:
		return jsonFloat(float64(t), 32)
	}
	if vr, ok := v.(driver.Valuer); ok {
		if dv, err := vr.Value(); err == nil && dv != nil {
			if s, ok := dv.(string); ok {
				// Оборачиваем в json.Number только если обрезанный текст —
				// корректное JSON-число; isDecimalStr принимает и формы, которые
				// json.Marshal отвергает (точка/ноль/плюс в начале).
				if n := trimNumeric(s); isDecimalStr(s) && json.Valid([]byte(n)) {
					return json.Number(n)
				}
				return s
			}
			// Рекурсия только на один уровень (см. cell): защита от цепочки Valuer.
			if _, again := dv.(driver.Valuer); !again {
				return jsonValue(dv)
			}
			return fmt.Sprintf("%v", dv)
		}
	}
	return v
}

// jsonFloat форматирует float для JSON. encoding/json не умеет NaN/±Inf, поэтому
// нечисловые значения становятся строками ("NaN"/"Infinity"/"-Infinity"), как и
// в to_json самого PostgreSQL. Конечные значения — json.Number без кавычек.
func jsonFloat(f float64, bits int) any {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return json.Number(formatFloat(f, bits))
}

// formatFloat повторяет psql: обычная десятичная запись для нормальных величин,
// экспонента только для очень больших/малых или нечисловых значений.
func formatFloat(f float64, bits int) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return strconv.FormatFloat(f, 'g', -1, bits)
	}
	if a := math.Abs(f); a != 0 && (a >= 1e15 || a < 1e-4) {
		return strconv.FormatFloat(f, 'g', -1, bits)
	}
	return strconv.FormatFloat(f, 'f', -1, bits)
}

// isDecimalStr сообщает, является ли s обычным десятичным числом (без экспоненты).
func isDecimalStr(s string) bool {
	if s == "" || strings.ContainsAny(s, "eE") {
		return false
	}
	dot := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c == '.':
			dot = true
		case (c == '-' || c == '+') && i == 0:
		default:
			return false
		}
	}
	return dot || strings.ContainsAny(s, "0123456789")
}

// trimNumeric убирает хвостовые нули (и хвостовую точку) у десятичного числа.
func trimNumeric(s string) string {
	if !strings.Contains(s, ".") || strings.ContainsAny(s, "eE") {
		return s
	}
	if !isDecimalStr(s) {
		return s
	}
	t := strings.TrimRight(s, "0")
	t = strings.TrimRight(t, ".")
	if t == "" || t == "-" || t == "+" {
		return "0"
	}
	return t
}

// numberRe сопоставляет обычный числовой литерал (со знаком, дробью и экспонентой
// опционально), чтобы csvSafe не трогал настоящие числа.
var numberRe = regexp.MustCompile(`^[-+]?[0-9]*\.?[0-9]+([eE][-+]?[0-9]+)?$`)

// csvSafe нейтрализует инъекцию формул в таблицах. Текстовая ячейка, которую
// таблица сочтёт формулой (начало с '=', '+', '-', '@', табуляции или CR),
// предваряется одинарной кавычкой и показывается буквально — кроме обычных чисел,
// чтобы числовой экспорт не пострадал ("-5" остаётся "-5").
//
// Ведущие пробелы и табы пропускаются при поиске первого значимого символа:
// многие табличные процессоры обрезают начальные пробелы, поэтому " =cmd()"
// тоже трактуется как формула и экранируется. Префикс ' добавляется к ИСХОДНОЙ
// ячейке, а не к обрезанной, чтобы её содержимое не менялось.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	// Сами по себе ведущие табуляция/CR — триггеры формулы (как раньше).
	if s[0] == '\t' || s[0] == '\r' {
		return "'" + s
	}
	// Первый значимый символ после ведущих пробелов/табов.
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i == len(s) {
		return s
	}
	switch s[i] {
	case '=', '+', '-', '@':
		// Числа не трогаем: проверяем и исходную ячейку, и её значимую часть
		// без ведущих пробелов (" -5" процессор обрежет до числа -5).
		if numberRe.MatchString(s) || numberRe.MatchString(s[i:]) {
			return s
		}
		return "'" + s
	}
	return s
}

// checkRowWidth проверяет, что число значений строки совпадает с числом столбцов.
// Рассинхрон (схема и строка разъехались) раньше молча терял лишние значения или
// добивал недостающие пустыми — теперь это явная ошибка, а не тихая порча данных.
func checkRowWidth(ncols, nrow int) error {
	if nrow != ncols {
		return fmt.Errorf("export: row has %d values but there are %d columns", nrow, ncols)
	}
	return nil
}

// WriteCSV пишет строку заголовка и строки данных.
func WriteCSV(w io.Writer, cols []string, rows [][]any) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(cols); err != nil {
		return err
	}
	rec := make([]string, len(cols))
	for _, row := range rows {
		if err := checkRowWidth(len(cols), len(row)); err != nil {
			return err
		}
		for i := range cols {
			rec[i] = csvSafe(cell(row[i]))
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// disambiguateColumns строит ключи для JSON-объекта: повторяющееся имя столбца
// получает суффикс "_N" (id, id_2, id_3), чтобы дубликаты столбцов из SELECT не
// терялись (в map остаётся только последнее значение). Суффикс увеличивается до
// свободного ключа, чтобы сгенерированный "id_2" не столкнулся с реальным
// столбцом с именем "id_2".
func disambiguateColumns(cols []string) []string {
	used := make(map[string]bool, len(cols))
	keys := make([]string, len(cols))
	for i, c := range cols {
		k := c
		for n := 2; used[k]; n++ {
			k = fmt.Sprintf("%s_%d", c, n)
		}
		used[k] = true
		keys[i] = k
	}
	return keys
}

// RowStreamer пишет строки в CSV или JSON по мере поступления, не держа весь
// результат в памяти — путь, по которому идёт \export, когда результат в памяти
// был усечён и запрос нужно перечитать целиком.
type RowStreamer interface {
	WriteRow(row []any) error
	Rows() int
	Close() error
}

// csvStream — потоковый писатель CSV (заголовок пишется при создании).
type csvStream struct {
	cw  *csv.Writer
	rec []string
	n   int
}

// NewCSVStream пишет строку заголовка и возвращает стример для строк данных.
func NewCSVStream(w io.Writer, cols []string) (RowStreamer, error) {
	cw := csv.NewWriter(w)
	if err := cw.Write(cols); err != nil {
		return nil, err
	}
	return &csvStream{cw: cw, rec: make([]string, len(cols))}, nil
}

func (s *csvStream) WriteRow(row []any) error {
	if err := checkRowWidth(len(s.rec), len(row)); err != nil {
		return err
	}
	for i := range s.rec {
		s.rec[i] = csvSafe(cell(row[i]))
	}
	s.n++
	return s.cw.Write(s.rec)
}

func (s *csvStream) Rows() int { return s.n }

func (s *csvStream) Close() error {
	s.cw.Flush()
	return s.cw.Error()
}

// jsonStream — потоковый писатель JSON-массива: по объекту на строку, выводится по
// мере поступления (ключи устраняют дубликаты как в WriteJSON и сортируются
// внутри объекта по алфавиту).
type jsonStream struct {
	w    io.Writer
	keys []string
	n    int
	err  error
}

// NewJSONStream открывает JSON-массив и возвращает стример для объектов строк.
func NewJSONStream(w io.Writer, cols []string) (RowStreamer, error) {
	if _, err := io.WriteString(w, "[\n"); err != nil {
		return nil, err
	}
	return &jsonStream{w: w, keys: disambiguateColumns(cols)}, nil
}

func (s *jsonStream) WriteRow(row []any) error {
	if s.err != nil {
		return s.err
	}
	if err := checkRowWidth(len(s.keys), len(row)); err != nil {
		s.err = err
		return err
	}
	obj := make(map[string]any, len(s.keys))
	for i, k := range s.keys {
		obj[k] = jsonValue(row[i])
	}
	b, err := json.Marshal(obj)
	if err != nil {
		s.err = err
		return err
	}
	sep := "  "
	if s.n > 0 {
		sep = ",\n  "
	}
	if _, err := io.WriteString(s.w, sep); err != nil {
		s.err = err
		return err
	}
	if _, err := s.w.Write(b); err != nil {
		s.err = err
		return err
	}
	s.n++
	return nil
}

func (s *jsonStream) Rows() int { return s.n }

func (s *jsonStream) Close() error {
	if s.err != nil {
		return s.err
	}
	_, err := io.WriteString(s.w, "\n]\n")
	return err
}

// Envelope — стабильный машиночитаемый конверт для headless-вывода (terox query
// --format envelope). В отличие от плоского массива объектов, он несёт схему,
// provenance и пер-шардовые ошибки/SQLSTATE, а строки даёт МАССИВАМИ значений
// (в порядке Columns), поэтому повторяющиеся имена столбцов не теряются и не
// конфликтуют как ключи объекта. schema_version фиксирует контракт для CI.
type Envelope struct {
	SchemaVersion int          `json:"schema_version"`
	Target        string       `json:"target,omitempty"`
	Columns       []string     `json:"columns"`
	Rows          [][]any      `json:"rows"`
	RowCount      int          `json:"row_count"`
	Shards        ShardSummary `json:"shards"`
	// Errors всегда присутствует (пустой массив при успехе) — стабильная схема.
	Errors []ShardError `json:"errors"`
	// Warnings — неблокирующие замечания (например, дрейф типов колонок между
	// шардами). Всегда присутствует (пустой массив при отсутствии).
	Warnings []string `json:"warnings"`
	// Truncated — true, если хотя бы один шард упёрся в лимит строк (результат неполный).
	Truncated bool `json:"truncated"`
	// Schema — типизированная схема объединённого результата (Feature 3): имя, OID,
	// человекочитаемое имя типа, typmod, номер вхождения, флаг synthetic. Всегда
	// присутствует (пустой массив при отсутствии). Позволяет CI проверять типы
	// колонок, а не только имена, и ловить серверный дрейф типов между прогонами.
	Schema []ColumnJSON `json:"schema"`
	// SchemaCheck — полнота проверки типов: complete|partial|skipped (см.
	// render.SchemaCheckStatus). Отличает «дрейфа нет» от «типы не снимались».
	SchemaCheck string `json:"schema_check,omitempty"`
	// ShardMeta — пер-шардовая provenance (Feature 13): версия сервера, backend PID,
	// длительность, SQLSTATE и статус. Всегда присутствует (пустой массив при отсутствии).
	ShardMeta []ShardMetaJSON `json:"shard_meta"`
}

// ShardMetaJSON — provenance одного шарда (Feature 13): откуда и как получен результат.
type ShardMetaJSON struct {
	Shard         string `json:"shard"`
	OK            bool   `json:"ok"`
	ServerVersion string `json:"server_version,omitempty"`
	BackendPID    uint32 `json:"backend_pid,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
	SQLState      string `json:"sqlstate,omitempty"`
}

// ColumnJSON — типизированная колонка в Envelope (Feature 3).
type ColumnJSON struct {
	Name       string `json:"name"`
	TypeOID    uint32 `json:"type_oid"`
	TypeName   string `json:"type_name"`
	Typmod     int32  `json:"typmod,omitempty"`
	Occurrence int    `json:"occurrence"`
	Synthetic  bool   `json:"synthetic,omitempty"`
}

// ShardSummary — сводка по шардам веера.
type ShardSummary struct {
	Total     int `json:"total"`
	OK        int `json:"ok"`
	Failed    int `json:"failed"`
	Truncated int `json:"truncated"`
}

// ShardError — ошибка одного шарда с SQLSTATE и severity отдельно от текста.
type ShardError struct {
	Shard    string `json:"shard"`
	SQLState string `json:"sqlstate,omitempty"`
	Severity string `json:"severity,omitempty"`
	Message  string `json:"message"`
	Kind     string `json:"kind,omitempty"`
}

// ShardResultJSON — результат одного шарда для per-shard вывода (Feature 13).
type ShardResultJSON struct {
	Shard         string      `json:"shard"`
	Columns       []string    `json:"columns"`
	Rows          [][]any     `json:"rows"`
	Truncated     bool        `json:"truncated"`
	ServerVersion string      `json:"server_version,omitempty"`
	BackendPID    uint32      `json:"backend_pid,omitempty"`
	DurationMS    int64       `json:"duration_ms"`
	Error         *ShardError `json:"error,omitempty"`
}

// PerShardEnvelope — машиночитаемый конверт режима per-shard: отдельный набор
// строк на каждый шард (без объединения колонок).
type PerShardEnvelope struct {
	SchemaVersion int               `json:"schema_version"`
	Target        string            `json:"target,omitempty"`
	Mode          string            `json:"mode"`
	Shards        ShardSummary      `json:"shards"`
	Results       []ShardResultJSON `json:"results"`
}

// WritePerShardEnvelope сериализует per-shard конверт со стабильными непустыми
// срезами.
func WritePerShardEnvelope(out io.Writer, env PerShardEnvelope) error {
	if env.Results == nil {
		env.Results = []ShardResultJSON{}
	}
	for i := range env.Results {
		if env.Results[i].Columns == nil {
			env.Results[i].Columns = []string{}
		}
		if env.Results[i].Rows == nil {
			env.Results[i].Rows = [][]any{}
		}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(env)
}

// RowValues приводит каждое значение строк к JSON-представлению (как jsonValue:
// []byte/uuid -> string, numeric -> json.Number, NaN/Inf -> строки), сохраняя
// порядок столбцов. Используется для Envelope.Rows.
func RowValues(rows [][]any) [][]any {
	out := make([][]any, len(rows))
	for i, row := range rows {
		vals := make([]any, len(row))
		for j, v := range row {
			if v == nil {
				vals[j] = nil
			} else {
				vals[j] = jsonValue(v)
			}
		}
		out[i] = vals
	}
	return out
}

// WriteEnvelope сериализует конверт с отступом. Гарантирует непустые срезы
// (columns/rows/errors), чтобы JSON-схема была стабильной (никогда не null).
func WriteEnvelope(w io.Writer, env Envelope) error {
	if env.Columns == nil {
		env.Columns = []string{}
	}
	if env.Rows == nil {
		env.Rows = [][]any{}
	}
	if env.Errors == nil {
		env.Errors = []ShardError{}
	}
	if env.Warnings == nil {
		env.Warnings = []string{}
	}
	if env.Schema == nil {
		env.Schema = []ColumnJSON{}
	}
	if env.ShardMeta == nil {
		env.ShardMeta = []ShardMetaJSON{}
	}
	env.RowCount = len(env.Rows)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(env)
}

// WriteJSON пишет массив объектов с ключами по именам столбцов. Дубликаты имён
// получают суффикс "_N", чтобы ни один столбец не потерялся.
func WriteJSON(w io.Writer, cols []string, rows [][]any) error {
	keys := disambiguateColumns(cols)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if err := checkRowWidth(len(keys), len(row)); err != nil {
			return err
		}
		obj := make(map[string]any, len(keys))
		for i, k := range keys {
			obj[k] = jsonValue(row[i])
		}
		out = append(out, obj)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
