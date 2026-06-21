// Package catalog содержит модель состояния сегментов версионированного снимка
// каталога БД (это и есть internal/catalog из плана аудита, раздел 4.2).
//
// Здесь живут per-segment load states и покрытие по шардам: каждый сегмент
// каталога (relations/schemas/search_path/keywords/functions/coverage/…) несёт
// собственное состояние загрузки, чтобы UI мог показать «catalog partial»,
// причину недоступности и покрытие по шардам, а не молча отсутствующий список
// (P2-5: ошибки больше не сворачиваются в пустой список).
//
// Богатый снимок Catalog/Relation/Column пока живёт в internal/complete и может
// быть перенесён сюда позже.
package catalog

import "time"

// Status — состояние загрузки сегмента каталога.
type Status int

const (
	StatusPending   Status = iota // ещё не загружался
	StatusLoaded                  // загружен полностью
	StatusPartial                 // загружен не на всех шардах
	StatusForbidden               // недостаточно прав (SQLSTATE 42501)
	StatusTimeout                 // превышен таймаут
	StatusFailed                  // прочая ошибка
)

func (s Status) String() string {
	switch s {
	case StatusLoaded:
		return "loaded"
	case StatusPartial:
		return "partial"
	case StatusForbidden:
		return "forbidden"
	case StatusTimeout:
		return "timeout"
	case StatusFailed:
		return "failed"
	default:
		return "pending"
	}
}

// LoadState — состояние одного сегмента каталога (relations/schemas/functions/…),
// чтобы UI мог показать «catalog partial», причину недоступности и покрытие по
// шардам, а не молча отсутствующий список.
type LoadState struct {
	Status   Status
	Error    string    // человекочитаемая причина при Status != loaded
	LoadedAt time.Time // когда сегмент последний раз пытались загрузить
	ShardsOK int       // шарды, успешно ответившие (для сегментов с покрытием)
	ShardsN  int       // всего шардов в попытке
}

// Coverage отрисовывает покрытие сегмента по шардам в виде "ShardsOK/ShardsN"
// (например "17/32"), когда покрытие отслеживалось (ShardsN > 0), иначе "".
func (l LoadState) Coverage() string {
	if l.ShardsN <= 0 {
		return ""
	}
	return itoa(l.ShardsOK) + "/" + itoa(l.ShardsN)
}

// Degraded сообщает, что сегмент не в полностью рабочем состоянии: он не загружен
// полностью и не ожидает загрузки (т.е. partial/forbidden/timeout/failed).
func (l LoadState) Degraded() bool {
	return l.Status != StatusLoaded && l.Status != StatusPending
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
