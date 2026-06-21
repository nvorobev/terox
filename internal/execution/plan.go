package execution

// ExecutionPlanner (аудит 4.1/4.2): единый слой ПРИНЯТИЯ РЕШЕНИЯ об исполнении.
// Главный архитектурный инвариант аудита — UI НЕ решает сам, можно ли выполнить
// SQL и какое подтверждение спросить. Раньше эта цепочка (write → нужен ли режим
// записи → не несёт ли тело своё BEGIN/COMMIT/SET ROLE → нет ли session-scoped
// состояния, переживающего COMMIT обёртки → какое подтверждение) была размазана
// по `repl.runStatement`. Теперь её владелец — `Plan(Request) Plan`, а REPL,
// headless и будущий API лишь ПОДЧИНЯЮТСЯ плану: рендерят отказ, спрашивают ровно
// то подтверждение, которое план потребовал, и исполняют.
//
// Этот файл — слой решения (risk/session/transaction policy). Timeout- и
// fan-out-policy уже реализованы ниже по стеку (`execWrite`/`cluster`) и здесь не
// дублируются.

import (
	"terox/internal/migration"
	"terox/internal/safety"
)

// Коды отказа — машиночитаемая причина, по которой запрос не исполняется до
// каких-либо подтверждений. UI выбирает по коду способ рендера (детальную
// справку), а не парсит текст.
const (
	// RefuseReadOnly — это запись, а режим записи (\write on) выключен.
	RefuseReadOnly = "read-only"
	// RefuseTxControl — тело несёт собственные BEGIN/COMMIT/ROLLBACK или SET ROLE,
	// что обошло бы защитную обёртку (set local role + statement/lock timeout).
	RefuseTxControl = "tx-control"
	// RefuseSessionState — session-scoped конструкция (SET search_path, TEMP,
	// LISTEN, PREPARE, cursor, session advisory lock, DISCARD) пережила бы COMMIT
	// обёртки и при transaction pooling утекла бы следующему клиенту.
	RefuseSessionState = "session-state"
)

// ConfirmLevel — требуемая сила подтверждения перед записью.
type ConfirmLevel int

const (
	// ConfirmNone — чтение: подтверждение не требуется.
	ConfirmNone ConfirmLevel = iota
	// ConfirmWrite — обычная запись (есть WHERE / конкретная цель).
	ConfirmWrite
	// ConfirmUnqualified — безусловная запись (UPDATE/DELETE без WHERE верхнего
	// уровня, TRUNCATE): усиленный барьер подтверждения.
	ConfirmUnqualified
)

// Refusal — отказ исполнить запрос: машиночитаемый код + дефолтное пояснение.
type Refusal struct {
	Code    string
	Message string
}

// Request — единичный введённый оператор и контекст его исполнения.
type Request struct {
	SQL       string
	WriteMode bool // включён ли \write
}

// Plan — решение планировщика по запросу. UI обязан ему подчиняться.
//
//   - Refusal != nil  → не исполнять; показать причину по Refusal.Code.
//   - IsWrite == false → чтение (execRead); подтверждение не требуется.
//   - IsWrite == true  → запись; перед execWrite показать Warnings и спросить
//     подтверждение уровня Confirm (если включён writeApprove).
type Plan struct {
	Decision safety.Decision // уровень риска и причины (для объяснения)
	IsWrite  bool
	Refusal  *Refusal     // != nil → запрос отклонён до исполнения
	Confirm  ConfirmLevel // требуемое подтверждение перед записью
	Warnings []string     // предупреждения перед записью (напр. волатильная функция)
}

// Refused сообщает, отклонён ли запрос до исполнения.
func (p Plan) Refused() bool { return p.Refusal != nil }

// Planner — единый ExecutionPlanner (аудит 4.1). Пока без состояния, но это шов
// для будущей политики (timeout/fan-out/target resolver): REPL/headless держат
// один планировщик и обращаются к нему вместо самостоятельных решений.
type Planner struct{}

// Plan строит план исполнения для одного запроса. Это ЕДИНЫЙ источник истины
// решения read-vs-write / refuse / confirm; порядок проверок повторяет защитный
// конвейер записи (режим записи → tx-control → session-state → подтверждение).
func (Planner) Plan(req Request) Plan {
	d := safety.Classify(req.SQL)
	p := Plan{Decision: d, IsWrite: d.Write}
	if !d.Write {
		return p // чтение — исполняется как есть
	}
	if !req.WriteMode {
		p.Refusal = &Refusal{
			Code:    RefuseReadOnly,
			Message: "read-only mode: this looks like a write. Enable with \\write on",
		}
		return p
	}
	// Тело со своим управлением транзакцией / сменой роли вышло бы из защитной
	// обёртки — отклоняем до всего остального (выполнить дословно можно через \i).
	if migration.HasTxControl(req.SQL) {
		p.Refusal = &Refusal{
			Code:    RefuseTxControl,
			Message: "statement carries its own BEGIN/COMMIT/ROLLBACK or SET ROLE",
		}
		return p
	}
	// Session-scoped состояние переживает COMMIT обёртки и утекает при transaction
	// pooling — reason уже называет конструкцию, причину и альтернативу.
	if reason := migration.SessionStateViolation(req.SQL); reason != "" {
		p.Refusal = &Refusal{Code: RefuseSessionState, Message: reason}
		return p
	}
	// Волатильная функция с побочным эффектом в SELECT (read-only транзакция её не
	// блокирует, P1-3) — самое неочевидное; выносим ВСЕ причины в предупреждение
	// (в скрипте может быть несколько волатильных функций).
	if d.Level == safety.RiskVolatileSideEffect && len(d.Reasons) > 0 {
		p.Warnings = append(p.Warnings, d.Reasons...)
	}
	if d.Unqualified {
		p.Confirm = ConfirmUnqualified
	} else {
		p.Confirm = ConfirmWrite
	}
	return p
}
