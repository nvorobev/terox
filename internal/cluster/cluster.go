// Package cluster разворачивает шаблоны хранилища в конкретные шарды и даёт
// обратный помощник (вывод шаблона и диапазона из первого/последнего хоста)
// для мастера регистрации.
package cluster

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"terox/internal/config"
)

// Shard — один разрешённый адрес подключения внутри хранилища.
type Shard struct {
	// Position — индекс шарда внутри хранилища, начиная с 0.
	Position int
	// Label — короткий читаемый id, например "rs001" или "shard_0".
	Label string
	// Host — разрешённое имя хоста.
	Host string
	// DB — разрешённое имя базы.
	DB       string
	Port     int
	User     string
	Password string
	SSLMode  string
	// Параметры профиля подключения (Feature 14); пустые опускаются в DSN.
	SSLRootCert    string
	SSLCert        string
	SSLKey         string
	ConnectTimeout time.Duration
	// PassFile — путь к libpq .pgpass (passfile= в DSN); секрет берётся оттуда,
	// если password пуст.
	PassFile string
}

// LabelDB форматирует шард как "label/db", если имя базы отличается от метки
// (например, при шардировании по хостам "rs002/shard_1"), иначе только метку
// (когда метка уже является базой). Используется в таблицах результатов и
// промпте, чтобы реальная база всегда была видна.
func (s Shard) LabelDB() string {
	if s.DB != "" && !strings.EqualFold(s.Label, s.DB) {
		return s.Label + "/" + s.DB
	}
	return s.Label
}

// placeholderRe соответствует {p}, {p1}, {p:03}, {p1:03}.
var placeholderRe = regexp.MustCompile(`\{(p1?)(?::(\d+))?\}`)

// resolveTemplate подставляет плейсхолдеры шарда для позиции p (с нуля).
func resolveTemplate(tmpl string, p int) string {
	return placeholderRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		groups := placeholderRe.FindStringSubmatch(m)
		base := groups[1]  // "p" или "p1"
		width := groups[2] // ширина дополнения, может быть пустой

		val := p
		if base == "p1" {
			val = p + 1
		}
		s := strconv.Itoa(val)
		if width != "" {
			w, _ := strconv.Atoi(width)
			if len(s) < w {
				s = strings.Repeat("0", w-len(s)) + s
			}
		}
		return s
	})
}

// hostLabel извлекает компактную метку из хоста (последний числоподобный
// сегмент до первой точки), с откатом к позиции.
func hostLabel(host string, p int) string {
	head := host
	if i := strings.IndexByte(head, '.'); i >= 0 {
		head = head[:i]
	}
	if i := strings.LastIndexByte(head, '-'); i >= 0 {
		return head[i+1:]
	}
	if head != "" {
		return head
	}
	return fmt.Sprintf("p%d", p)
}

// Expand разрешает все шарды хранилища.
func Expand(st *config.Storage) ([]Shard, error) {
	if st.HostTemplate == "" {
		return nil, fmt.Errorf("storage has empty host_template")
	}
	count := st.Count
	if count <= 0 {
		count = 1
	}
	sslmode := st.SSLMode
	if sslmode == "" {
		sslmode = "disable"
	}
	// Секрет из переменной окружения имеет приоритет над открытым password в YAML
	// (password_env). Пустая переменная — пустой пароль (как и отсутствие ключа).
	password := st.Password
	if st.PasswordEnv != "" {
		password = os.Getenv(st.PasswordEnv)
	}
	// connect_timeout округляем до целых секунд (libpq принимает только секунды),
	// чтобы DSN и ключ пула совпадали и не плодили идентичные пулы.
	connectTimeout := time.Duration(st.ConnectTimeout)
	if connectTimeout > 0 && connectTimeout < time.Second {
		connectTimeout = time.Second
	} else if connectTimeout > 0 {
		connectTimeout = connectTimeout.Truncate(time.Second)
	}
	db := st.DBTemplate
	if db == "" {
		db = "master"
	}
	// Для метки берём компонент, который реально меняется между шардами: суффикс
	// хоста, если шардируется шаблон хоста (rs001..), иначе имя базы, когда
	// меняется только база (shard_0..) — так метки остаются уникальными.
	hostSharded := placeholderRe.MatchString(st.HostTemplate)
	dbSharded := placeholderRe.MatchString(db)
	shards := make([]Shard, 0, count)
	seen := make(map[string]bool, count)
	// labelCount считает, сколько шардов претендуют на одну и ту же метку. Метка
	// используется как ключ выбора (ParseSelector) и адресации (labelToShard в
	// rollout, ledger миграций) — при коллизии запрос по метке молча ушёл бы в
	// первый совпавший шард, что опасно для записи/миграции. Поэтому при дубле
	// метку делаем уникальной суффиксом "#<позиция>" ниже.
	labelCount := make(map[string]int, count)
	for p := 0; p < count; p++ {
		host := resolveTemplate(st.HostTemplate, p)
		resolvedDB := resolveTemplate(db, p)
		// Отклоняем хранилище, чьи шарды сводятся к одному физическому адресу: при
		// count>1 и без {p}/{p1} в шаблонах все позиции дают один host/db, и
		// fan-out запись попала бы в одну базу N раз. Это всегда ошибка
		// конфигурации — падаем явно, не дублируя молча.
		key := host + "\x00" + strconv.Itoa(st.Port) + "\x00" + resolvedDB
		if seen[key] {
			return nil, fmt.Errorf("count=%d produces duplicate target %s:%d/%s — add a {p}/{p1} placeholder to host_template or db_template so each shard is distinct", count, host, st.Port, resolvedDB)
		}
		seen[key] = true
		label := hostLabel(host, p)
		if !hostSharded && dbSharded {
			label = resolvedDB
		}
		labelCount[label]++
		shards = append(shards, Shard{
			Position:       p,
			Label:          label,
			Host:           host,
			DB:             resolvedDB,
			Port:           st.Port,
			User:           st.User,
			Password:       password,
			SSLMode:        sslmode,
			SSLRootCert:    config.ExpandUserPath(st.SSLRootCert),
			SSLCert:        config.ExpandUserPath(st.SSLCert),
			SSLKey:         config.ExpandUserPath(st.SSLKey),
			ConnectTimeout: connectTimeout,
			PassFile:       config.ExpandUserPath(st.PassFile),
		})
	}
	disambiguateLabels(shards, labelCount)
	return shards, nil
}

// disambiguateLabels детерминированно делает метки уникальными, не трогая
// неконфликтный случай. Метки, встречающиеся более одного раза (две разные
// физические цели свелись к одинаковому суффиксу хоста, например "pg-{p}-rs.db"
// → метка "rs" у всех), получают суффикс "#<позиция>". Уникальные метки
// остаются как есть, чтобы привычный формат ("rs001", "shard_0") не менялся.
// Суффикс с позицией стабилен между запусками и сам не порождает новых
// коллизий: позиция уникальна, а Expand уже гарантировал уникальность целей.
func disambiguateLabels(shards []Shard, labelCount map[string]int) {
	for i := range shards {
		if labelCount[shards[i].Label] > 1 {
			shards[i].Label = fmt.Sprintf("%s#%d", shards[i].Label, shards[i].Position)
		}
	}
}

var numRe = regexp.MustCompile(`\d+`)

// DerivedTemplate — результат сравнения пары первый/последний хост.
type DerivedTemplate struct {
	HostTemplate string // с подставленным плейсхолдером {p1[:width]}
	Start        int    // номер первого хоста, начиная с 1
	Count        int    // число шардов (last-first+1)
	Width        int    // ширина дополнения нулями (0 = без)
}

// DeriveHostTemplate анализирует первый и последний хосты, отличающиеся только
// числовым сегментом, и возвращает шаблон хоста с подразумеваемым диапазоном.
//
// Пример: "pg-rs001.db" / "pg-rs128.db" ->
//
//	HostTemplate "pg-rs{p1:03}.db", Start 1, Count 128, Width 3
func DeriveHostTemplate(first, last string) (DerivedTemplate, error) {
	// Один хост (без диапазона): литерал как шаблон, один шард.
	if first == last {
		return DerivedTemplate{HostTemplate: first, Start: 1, Count: 1, Width: 0}, nil
	}
	firstNums := numRe.FindAllStringIndex(first, -1)
	lastNums := numRe.FindAllStringIndex(last, -1)
	if len(firstNums) == 0 {
		return DerivedTemplate{}, fmt.Errorf("first host %q has no numeric segment", first)
	}
	if len(firstNums) != len(lastNums) {
		return DerivedTemplate{}, fmt.Errorf("hosts %q and %q have different shapes", first, last)
	}

	// Находим единственный числовой сегмент, отличающийся у first и last.
	diffIdx := -1
	for i := range firstNums {
		fSeg := first[firstNums[i][0]:firstNums[i][1]]
		lSeg := last[lastNums[i][0]:lastNums[i][1]]
		// Префикс/суффикс вокруг сегмента должны совпадать для чистого шаблона.
		if first[:firstNums[i][0]] != last[:lastNums[i][0]] {
			continue
		}
		if fSeg != lSeg {
			if diffIdx != -1 {
				return DerivedTemplate{}, fmt.Errorf("hosts differ in more than one numeric segment")
			}
			diffIdx = i
		}
	}
	if diffIdx == -1 {
		// Выше гарантировано first != last, значит они отличаются только
		// нечисловым сегментом — это не числовой диапазон для шаблона.
		return DerivedTemplate{}, fmt.Errorf("hosts %q and %q differ only in a non-numeric segment", first, last)
	}

	seg := firstNums[diffIdx]
	firstNum := first[seg[0]:seg[1]]
	lastSeg := lastNums[diffIdx]
	lastNum := last[lastSeg[0]:lastSeg[1]]
	// Текст после различающегося числа должен совпадать (префикс уже совпал).
	if first[seg[1]:] != last[lastSeg[1]:] {
		return DerivedTemplate{}, fmt.Errorf("hosts %q and %q differ outside the numeric segment", first, last)
	}

	start, err := strconv.Atoi(firstNum)
	if err != nil {
		return DerivedTemplate{}, err
	}
	end, err := strconv.Atoi(lastNum)
	if err != nil {
		return DerivedTemplate{}, err
	}
	if end < start {
		return DerivedTemplate{}, fmt.Errorf("last index %d is before first index %d", end, start)
	}

	width := padWidth(firstNum)
	// Шаблон использует плейсхолдер {p1} (с 1), дающий значения 1..Count. Это
	// подходит реальным кластерам (хосты начинаются с rs001 / rs01). Если хосты
	// начинаются с другого числа, Start показывается мастеру для превью и
	// предупреждения; сохранённый шаблон всё равно начинается с 1.
	tmpl := first[:seg[0]] + placeholder(width) + first[seg[1]:]

	return DerivedTemplate{
		HostTemplate: tmpl,
		Start:        start,
		Count:        end - start + 1,
		Width:        width,
	}, nil
}

// placeholder строит плейсхолдер {p1}, кодируя ширину дополнения нулями при width > 0.
func placeholder(width int) string {
	if width > 0 {
		return fmt.Sprintf("{p1:%02d}", width)
	}
	return "{p1}"
}

// padWidth возвращает ширину дополнения нулями по числовому литералу (0, если
// у литерала нет ведущего нуля).
func padWidth(lit string) int {
	if len(lit) > 1 && lit[0] == '0' {
		return len(lit)
	}
	return 0
}
