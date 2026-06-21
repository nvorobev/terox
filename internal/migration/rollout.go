package migration

// Feature 11: планировщик поэтапной раскатки миграций. Чистая (без БД) логика
// разбиения целей на этапы: canary (один шард) → батчи фиксированного размера, с
// учётом уже применённых шардов (resume только незавершённых). Сам прогон с барьерами
// между батчами выполняет REPL; здесь — детерминированное планирование и структурный
// отчёт для CI.

// RolloutPlan описывает порядок раскатки: список этапов, где каждый этап — набор
// меток шардов, применяемых вместе.
type RolloutPlan struct {
	Pending []string   // шарды, ещё не применённые (в порядке)
	Skipped []string   // шарды, уже применённые ранее (пропускаются при resume)
	Stages  [][]string // этапы: [canary], затем батчи
}

// PlanRollout строит план раскатки. allShards — все выбранные шарды (в порядке);
// appliedSet — метки уже применённых (resume их пропускает, если resume=true).
// canary=true делает первый этап одиночным (один шард для проверки). batchSize<=0
// означает «все оставшиеся одним этапом».
func PlanRollout(allShards []string, appliedSet map[string]bool, resume, canary bool, batchSize int) RolloutPlan {
	var p RolloutPlan
	seen := make(map[string]bool, len(allShards))
	for _, s := range allShards {
		if seen[s] {
			continue // дубликат метки: миграция применяется к шарду ровно один раз
		}
		seen[s] = true
		if resume && appliedSet[s] {
			p.Skipped = append(p.Skipped, s)
			continue
		}
		p.Pending = append(p.Pending, s)
	}
	rest := p.Pending
	if canary && len(rest) > 0 {
		p.Stages = append(p.Stages, []string{rest[0]})
		rest = rest[1:]
	}
	if batchSize <= 0 {
		if len(rest) > 0 {
			p.Stages = append(p.Stages, rest)
		}
		return p
	}
	for len(rest) > 0 {
		n := min(batchSize, len(rest))
		p.Stages = append(p.Stages, rest[:n])
		rest = rest[n:]
	}
	return p
}

// ShardOutcome — итог применения миграции на одном шарде (для отчёта/ledger).
// (Статус verified производит будущая verify-фаза — task F11+.)
type ShardOutcome struct {
	Shard        string `json:"shard"`
	Status       string `json:"status"` // applied|failed|verified|pending|skipped
	SQLState     string `json:"sqlstate,omitempty"`
	Error        string `json:"error,omitempty"`
	AffectedRows int64  `json:"affected_rows"`
	DurationMS   int64  `json:"duration_ms"`
}

// RolloutReport — машиночитаемый итог раскатки (для CI/автоматизации).
type RolloutReport struct {
	Migration  string         `json:"migration"`
	Checksum   string         `json:"checksum"`
	Context    string         `json:"context"`
	Operator   string         `json:"operator,omitempty"`
	TeroxVer   string         `json:"terox_version,omitempty"`
	StartedAt  string         `json:"started_at,omitempty"`
	FinishedAt string         `json:"finished_at,omitempty"`
	Mode       string         `json:"mode"` // transactional|non-transactional|pass-through
	Stages     int            `json:"stages"`
	Shards     []ShardOutcome `json:"shards"`
}

// Summary считает итоги по статусам для краткой строки/проверок.
func (r RolloutReport) Summary() (applied, failed, pending, skipped int) {
	for _, s := range r.Shards {
		switch s.Status {
		case "applied", "verified":
			applied++
		case "failed":
			failed++
		case "pending":
			pending++
		case "skipped":
			skipped++
		}
	}
	return
}

// AppliedSetFromShards строит множество меток из карты ledger (shard→ts).
func AppliedSetFromShards(shards map[string]string) map[string]bool {
	set := make(map[string]bool, len(shards))
	for s := range shards {
		set[s] = true
	}
	return set
}
