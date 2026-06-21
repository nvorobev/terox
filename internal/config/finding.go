// Package config — структурированная модель находок валидации/preflight.
package config

// Severity — серьёзность находки preflight.
type Severity int

const (
	// SeverityWarning — вероятная ошибка настройки, которая не блокирует старт
	// (но блокирует под --strict, если код не в allow-list).
	SeverityWarning Severity = iota
	// SeverityError — блокирующая ошибка: ни один путь, создающий соединение с БД,
	// не должен стартовать, пока она не устранена.
	SeverityError
)

func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	default:
		return "unknown"
	}
}

// Finding — одна находка preflight со СТАБИЛЬНЫМ машиночитаемым кодом, серьёзностью,
// путём (service/storage или пусто для глобальных) и человекочитаемым сообщением.
// Код — контракт для CI: его можно адресно подавлять через --allow-warning и
// проверять в автоматизации, не парся текст сообщения.
type Finding struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Path     string   `json:"path,omitempty"`
	Message  string   `json:"message"`
}

// IsError сообщает, является ли находка блокирующей ошибкой.
func (f Finding) IsError() bool { return f.Severity == SeverityError }

// splitFindings разбивает находки на ошибки и предупреждения (как строки сообщений),
// сохраняя порядок — это обеспечивает совместимость со старым Validate() (errs, warns).
func splitFindings(fs []Finding) (errs, warns []string) {
	for _, f := range fs {
		if f.IsError() {
			errs = append(errs, f.Message)
		} else {
			warns = append(warns, f.Message)
		}
	}
	return errs, warns
}
