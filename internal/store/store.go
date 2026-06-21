// Package store хранит именованные запросы и журнал применённых миграций
// по шардам в каталоге конфигурации terox.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// writeFileAtomic пишет данные во временный файл в том же каталоге и
// переименовывает его поверх path, чтобы сбой при записи не оставил
// частичный файл. Важно для журнала миграций (applied.json).
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // после успешного переименования ничего не делает
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func dir() (string, error) {
	d := os.Getenv("XDG_CONFIG_HOME")
	if d == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		d = filepath.Join(home, ".config")
	}
	d = filepath.Join(d, "terox")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// Queries — хранилище имя -> SQL, сохраняемое в YAML.
type Queries struct {
	path string
	M    map[string]string `yaml:"queries"`
}

// LoadQueries читает файл сохранённых запросов (пустое хранилище, если файла нет).
func LoadQueries() (*Queries, error) {
	d, err := dir()
	if err != nil {
		return nil, err
	}
	q := &Queries{path: filepath.Join(d, "queries.yaml"), M: map[string]string{}}
	data, err := os.ReadFile(q.path)
	if err != nil {
		if os.IsNotExist(err) {
			return q, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(data, q); err != nil {
		return nil, err
	}
	if q.M == nil {
		q.M = map[string]string{}
	}
	return q, nil
}

func (q *Queries) save() error {
	data, err := yaml.Marshal(q)
	if err != nil {
		return err
	}
	return writeFileAtomic(q.path, data, 0o600)
}

// Set сохраняет запрос и записывает на диск.
func (q *Queries) Set(name, sql string) error {
	q.M[name] = sql
	return q.save()
}

// Get возвращает сохранённый запрос.
func (q *Queries) Get(name string) (string, bool) {
	s, ok := q.M[name]
	return s, ok
}

// Delete удаляет сохранённый запрос и записывает на диск.
func (q *Queries) Delete(name string) error {
	delete(q.M, name)
	return q.save()
}

// Names возвращает отсортированный список имён сохранённых запросов.
func (q *Queries) Names() []string {
	out := make([]string, 0, len(q.M))
	for n := range q.M {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Applied — журнал применённых миграций по шардам, с ключами
// "service/storage" -> миграция -> метка шарда -> метка времени. Контрольные
// суммы содержимого миграций хранятся ОТДЕЛЬНО (checksums.json), чтобы формат
// applied.json не менялся: ключ "service/storage" -> миграция -> sha256.
type Applied struct {
	path  string
	cpath string
	M     map[string]map[string]map[string]string
	C     map[string]map[string]string
}

// LoadApplied читает журнал применённых миграций (пустой, если файла нет).
func LoadApplied() (*Applied, error) {
	d, err := dir()
	if err != nil {
		return nil, err
	}
	a := &Applied{
		path:  filepath.Join(d, "applied.json"),
		cpath: filepath.Join(d, "checksums.json"),
		M:     map[string]map[string]map[string]string{},
		C:     map[string]map[string]string{},
	}
	data, err := os.ReadFile(a.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		if err := json.Unmarshal(data, &a.M); err != nil {
			return nil, err
		}
		if a.M == nil {
			a.M = map[string]map[string]map[string]string{}
		}
	}
	if cdata, err := os.ReadFile(a.cpath); err == nil {
		if err := json.Unmarshal(cdata, &a.C); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if a.C == nil {
		a.C = map[string]map[string]string{}
	}
	return a, nil
}

func (a *Applied) save() error {
	data, err := json.MarshalIndent(a.M, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.path, data, 0o600)
}

// Record отмечает миграцию применённой на шарде в момент ts и записывает на диск.
func (a *Applied) Record(ctxKey, migration, shard, ts string) error {
	if a.M[ctxKey] == nil {
		a.M[ctxKey] = map[string]map[string]string{}
	}
	if a.M[ctxKey][migration] == nil {
		a.M[ctxKey][migration] = map[string]string{}
	}
	a.M[ctxKey][migration][shard] = ts
	return a.save()
}

// RecordChecksum сохраняет sha256 содержимого миграции для контекста (в
// checksums.json). Nil-безопасна для тестовых Applied без инициализации.
func (a *Applied) RecordChecksum(ctxKey, migration, checksum string) error {
	if a.C == nil {
		a.C = map[string]map[string]string{}
	}
	if a.C[ctxKey] == nil {
		a.C[ctxKey] = map[string]string{}
	}
	a.C[ctxKey][migration] = checksum
	if a.cpath == "" {
		return nil
	}
	data, err := json.MarshalIndent(a.C, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.cpath, data, 0o600)
}

// Checksum возвращает ранее записанную контрольную сумму миграции (и был ли
// записан хоть какой-то), чтобы обнаружить повторное применение с ИЗМЕНЁННЫМ
// содержимым.
func (a *Applied) Checksum(ctxKey, migration string) (string, bool) {
	if a.C[ctxKey] == nil {
		return "", false
	}
	c, ok := a.C[ctxKey][migration]
	return c, ok
}

// Shards возвращает метка шарда -> метка времени для миграции в контексте.
func (a *Applied) Shards(ctxKey, migration string) map[string]string {
	if a.M[ctxKey] == nil {
		return nil
	}
	return a.M[ctxKey][migration]
}

// Migrations возвращает отсортированный список имён миграций для контекста.
func (a *Applied) Migrations(ctxKey string) []string {
	m := a.M[ctxKey]
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
