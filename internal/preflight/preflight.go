// Package preflight — единый предполётный контроль конфига для ВСЕХ точек входа,
// создающих соединение с БД (REPL, query, plan, add). Композитит валидацию конфига
// (config.Findings) с разворачиванием хранилищ (cluster.Expand), которое требует
// пакета cluster, и потому не может жить внутри пакета config (цикл импорта).
//
// Это единственный источник истины preflight: главное правило аудита — UI не решает
// сам, безопасен ли запуск; все точки входа спрашивают один и тот же Gate.
package preflight

import (
	"fmt"
	"sort"
	"strings"

	"terox/internal/cluster"
	"terox/internal/config"
)

// Options управляет решением Gate.
type Options struct {
	// Strict превращает предупреждения в блокирующие (кроме подавленных AllowWarning).
	Strict bool
	// AllowEmpty разрешает старт с пустым конфигом (интерактивный REPL открывает мастер).
	AllowEmpty bool
	// AllowWarning — множество кодов предупреждений, которые осознанно приняты:
	// они не считаются под --strict и помечаются как override, а не как обычный warn.
	AllowWarning map[string]bool
}

// Run возвращает ВСЕ находки preflight: валидацию конфига плюс по находке на каждое
// хранилище, которое не разворачивается в различимые шарды (count>1 без плейсхолдера →
// дублирующиеся цели). Порядок детерминирован.
func Run(cfg *config.Config) []config.Finding {
	fs := cfg.Findings()
	for _, svc := range cfg.ServiceNames() {
		for _, sto := range cfg.StorageNames(svc) {
			st := cfg.Services[svc].Storages[sto]
			if st == nil {
				// Уже сообщено в Findings; не зовём Expand на nil.
				continue
			}
			if _, err := cluster.Expand(st); err != nil {
				id := svc + "/" + sto
				fs = append(fs, config.Finding{
					Code:     "storage-expand",
					Severity: config.SeverityError,
					Path:     id,
					Message:  fmt.Sprintf("%s: %v", id, err),
				})
			}
		}
	}
	return fs
}

// Partition делит находки на блокирующие ошибки, активные предупреждения (severity
// warning, код НЕ в AllowWarning) и осознанно принятые override-предупреждения (код
// в AllowWarning). Ошибки нельзя подавить через AllowWarning.
func Partition(fs []config.Finding, allow map[string]bool) (errs, warns, overrides []config.Finding) {
	for _, f := range fs {
		switch {
		case f.IsError():
			errs = append(errs, f)
		case allow[f.Code]:
			overrides = append(overrides, f)
		default:
			warns = append(warns, f)
		}
	}
	return errs, warns, overrides
}

// Gate принимает решение о запуске: nil — продолжать, ошибка — отказать. Блокирует
// при любой ошибке (если конфиг не пуст и не allowEmpty), а под Strict — ещё и при
// любом НЕподавленном предупреждении. allowEmpty+пустой конфиг пропускает обе блокировки,
// чтобы интерактивный REPL мог открыть мастер настройки.
func Gate(cfg *config.Config, fs []config.Finding, opt Options) error {
	errs, warns, _ := Partition(fs, opt.AllowWarning)
	// nil-конфиг трактуем как пустой, чтобы не разыменовать его в IsEmpty (под
	// AllowEmpty это штатный путь — REPL откроет мастер настройки).
	bypass := opt.AllowEmpty && (cfg == nil || cfg.IsEmpty())
	if len(errs) > 0 && !bypass {
		return fmt.Errorf("refusing to start with %d config error(s) — fix them or run 'terox validate'", len(errs))
	}
	if opt.Strict && len(warns) > 0 && !bypass {
		return fmt.Errorf("refusing to start: %d config warning(s) under --strict", len(warns))
	}
	return nil
}

// UnknownAllowCodes возвращает коды из allow-list, которых нет среди известных кодов
// валидации — помогает поймать опечатку в --allow-warning CODE. known строится из
// фактических находок плюс полного реестра кодов (AllCodes).
func UnknownAllowCodes(allow map[string]bool) []string {
	known := AllCodes()
	var unknown []string
	for code := range allow {
		if !known[code] {
			unknown = append(unknown, code)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// AllCodes — реестр всех кодов находок, которые умеют выдавать config.Findings и Run.
// Используется для валидации --allow-warning CODE (опечатки) и документации.
func AllCodes() map[string]bool {
	codes := []string{
		"no-services", "service-no-storages", "storage-empty", "host-template-empty",
		"port-invalid", "user-empty", "count-negative", "count-no-placeholder",
		"password-env-empty", "password-placeholder",
		"connect-timeout-negative",
		"ssl-rootcert-missing", "client-cert-pair", "profile-file-unreadable",
		"prod-insecure-tls", "prod-insecure-tls-allowed", "prod-require-unverified",
		"pg-duration-invalid", "pg-duration-subms", "query-timeout-low",
		"fanout-mode-invalid", "write-error-mode-invalid", "max-rows-negative",
		"fanout-concurrency-negative", "query-timeout-negative", "migration-timeout-negative",
		"write-mode-default-prod", "config-perms", "storage-expand",
	}
	m := make(map[string]bool, len(codes))
	for _, c := range codes {
		m[c] = true
	}
	return m
}

// ParseAllowWarning превращает повторяемый флаг --allow-warning в множество кодов,
// принимая также запятую как разделитель (--allow-warning a,b).
func ParseAllowWarning(values []string) map[string]bool {
	allow := map[string]bool{}
	for _, v := range values {
		for code := range strings.SplitSeq(v, ",") {
			if c := strings.TrimSpace(code); c != "" {
				allow[c] = true
			}
		}
	}
	return allow
}
