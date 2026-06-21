package repl

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"terox/internal/cluster"
	"terox/internal/db"
	"terox/internal/execution"
	"terox/internal/migration"
	"terox/internal/ui"
)

// replicationLagSQL измеряет максимальную задержку применения WAL на стендбаях,
// подключённых к ПЕРВИЧНОМУ (выполняется на шарде = первичном). Нет реплик → 0.
const replicationLagSQL = `SELECT coalesce(max(extract(epoch FROM replay_lag))::float8, 0) FROM pg_stat_replication`

// maxReplayLagSeconds извлекает максимальную задержку репликации (сек) из результатов
// веера (одна числовая колонка на шард). ok=false, если ни один шард не дал значения
// (нет прав/нет колонки/ошибка) — тогда lag-gating консервативно НЕ срабатывает.
func maxReplayLagSeconds(results []db.ShardResult) (float64, bool) {
	maxLag, any := 0.0, false
	for _, sr := range results {
		if sr.Err != nil || sr.Result == nil || len(sr.Result.Rows) == 0 || len(sr.Result.Rows[0]) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(str(sr.Result.Rows[0][0])), 64)
		if err != nil {
			continue
		}
		any = true
		if v > maxLag {
			maxLag = v
		}
	}
	return maxLag, any
}

// replicationLagExceeded измеряет задержку репликации на текущих целях и сообщает,
// превышает ли она порог. При недоступности метрики (ok=false) НЕ блокирует.
func (r *REPL) replicationLagExceeded(maxLag time.Duration) (time.Duration, bool) {
	secs, ok := maxReplayLagSeconds(r.fanoutRead(replicationLagSQL))
	if !ok {
		return 0, false
	}
	d := time.Duration(secs * float64(time.Second))
	return d, d > maxLag
}

// Feature 11: поэтапная раскатка миграции (canary → батчи) с барьерами, resume
// только незавершённых шардов и машиночитаемым отчётом. Планирование — чистый
// migration.PlanRollout; здесь — оркестрация прогона поверх execWrite/recordApplied.

// buildVersion — версия сборки terox для машиночитаемого отчёта раскатки. Задаётся
// из main через SetVersion (иначе пусто).
var buildVersion string

// SetVersion сообщает пакету версию сборки (вызывается из main).
func SetVersion(v string) { buildVersion = v }

// checksumMismatch сообщает, применялась ли миграция с этим именем ранее с ДРУГИМ
// содержимым (sha256 не совпадает) — основание заблокировать без --force.
func (r *REPL) checksumMismatch(name, content string) bool {
	if r.applied == nil || name == "" {
		return false
	}
	prev, ok := r.applied.Checksum(r.service+"/"+r.storage, name)
	return ok && prev != migration.Checksum(content)
}

// previewRollout печатает план раскатки в dry-run.
func (r *REPL) previewRollout(o migrateOpts) {
	labels := make([]string, len(r.targets))
	for i, s := range r.targets {
		labels[i] = s.Label
	}
	applied := map[string]bool{}
	if r.applied != nil {
		applied = migration.AppliedSetFromShards(r.applied.Shards(r.service+"/"+r.storage, fileBase(o.path)))
	}
	pl := migration.PlanRollout(labels, applied, o.resume, o.canary, o.batch)
	fmt.Fprintf(r.out, "rollout plan: %d pending, %d skipped, %d stage(s)\n", len(pl.Pending), len(pl.Skipped), len(pl.Stages))
	for i, stage := range pl.Stages {
		kind := "batch"
		if i == 0 && o.canary {
			kind = "canary"
		}
		fmt.Fprintf(r.out, "  stage %d (%s): %s\n", i+1, kind, strings.Join(stage, ", "))
	}
}

// runStagedRollout применяет миграцию этапами с барьером между ними, помечает
// незавершённые шарды pending при паузе/ошибке (resume их доберёт) и печатает
// машиночитаемый отчёт.
func (r *REPL) runStagedRollout(wrap bool, content, name string, plan migration.Plan, o migrateOpts) error {
	labels := make([]string, len(r.targets))
	labelToShard := map[string]cluster.Shard{}
	for i, s := range r.targets {
		labels[i] = s.Label
		labelToShard[s.Label] = s
	}
	applied := map[string]bool{}
	if r.applied != nil {
		applied = migration.AppliedSetFromShards(r.applied.Shards(r.service+"/"+r.storage, name))
	}
	pl := migration.PlanRollout(labels, applied, o.resume, o.canary, o.batch)
	if len(pl.Pending) == 0 {
		fmt.Fprintf(r.out, "all %d target shard(s) already applied for %s — nothing to roll out\n", len(labels), name)
		return nil
	}
	if len(pl.Skipped) > 0 {
		fmt.Fprintf(r.out, "resume: skipping %d already-applied shard(s): %s\n", len(pl.Skipped), strings.Join(pl.Skipped, ", "))
	}
	fmt.Fprintf(r.out, "rollout %s → %d pending shard(s) in %d stage(s) [%s]\n", name, len(pl.Pending), len(pl.Stages), r.targetLabel)

	if r.writeApprove {
		var ok bool
		if execution.AnyUnqualifiedWrite(content) {
			ok = r.confirmUnqualified()
		} else {
			ok = r.confirmWrite()
		}
		if !ok {
			fmt.Fprintln(r.out, "cancelled")
			return nil
		}
	}

	saved := r.targets
	defer func() { r.targets = saved }()

	report := migration.RolloutReport{
		Migration: name, Checksum: migration.Checksum(content),
		Context: r.service + "/" + r.storage, Operator: os.Getenv("USER"),
		TeroxVer: buildVersion, StartedAt: r.now(), Mode: rolloutMode(plan, wrap), Stages: len(pl.Stages),
	}
	for _, s := range pl.Skipped {
		report.Shards = append(report.Shards, migration.ShardOutcome{Shard: s, Status: "skipped"})
	}

	paused := false
	for i, stage := range pl.Stages {
		if paused {
			for _, lbl := range stage {
				report.Shards = append(report.Shards, migration.ShardOutcome{Shard: lbl, Status: "pending"})
			}
			continue
		}
		// Барьер: подтверждение перед каждым этапом, кроме первого.
		if i > 0 {
			if !r.confirmYes(fmt.Sprintf("stage %d/%d (%d shard(s)) — continue? [y/N] ", i+1, len(pl.Stages), len(stage))) {
				fmt.Fprintln(r.out, "paused — remaining shards left pending (re-run with --resume to continue)")
				for _, lbl := range stage {
					report.Shards = append(report.Shards, migration.ShardOutcome{Shard: lbl, Status: "pending"})
				}
				paused = true
				continue
			}
		}
		r.targets = shardsForLabels(stage, labelToShard)
		kind := "batch"
		if i == 0 && o.canary {
			kind = "canary"
		}
		fmt.Fprintf(r.out, "— stage %d/%d (%s): %s\n", i+1, len(pl.Stages), kind, strings.Join(stage, ", "))
		results, err := r.execWrite(content, wrap)
		if err != nil {
			// Этап ОТКЛОНЁН до выполнения (запрещённая операция, нетранзакционная
			// миграция на prod без дедлайна, отмена 'unprotected' и т.п.). Раньше
			// execWrite возвращал пустой список без сигнала, и раскатка засчитывала
			// «applied 0, failed 0, pending 0» как успех. Теперь шарды этапа явно
			// помечаются pending, раскатка встаёт на паузу (resume их доберёт), а
			// итоговый отчёт получит ненулевой pending → функция вернёт ошибку.
			fmt.Fprintln(r.out, ui.Danger.Render("⚠ stage refused before execution — pausing rollout; resolve the cause and re-run with --resume"))
			for _, lbl := range stage {
				report.Shards = append(report.Shards, migration.ShardOutcome{Shard: lbl, Status: "pending", Error: oneLine(err.Error())})
			}
			paused = true
			continue
		}
		r.recordApplied(name, content, results)
		anyFail := false
		for _, res := range results {
			oc := migration.ShardOutcome{Shard: res.Shard.Label, AffectedRows: res.Affected, DurationMS: res.Duration.Milliseconds(), Status: "applied"}
			if res.Err != nil {
				oc.Status = "failed"
				oc.Error = oneLine(res.Err.Error())
				oc.SQLState = db.ClassifyError(res.Err).SQLState
				anyFail = true
			}
			report.Shards = append(report.Shards, oc)
		}
		if anyFail {
			fmt.Fprintln(r.out, ui.Danger.Render("⚠ stage failed — pausing rollout; fix the cause and re-run with --resume to continue"))
			paused = true
		}
		// Lag-gating: после успешного этапа и перед следующим проверяем задержку
		// репликации; превышение порога ставит раскатку на паузу, чтобы стендбаи
		// догнали первичные до следующего батча (F11+).
		if !anyFail && o.maxLag > 0 && i < len(pl.Stages)-1 {
			if lag, exceeded := r.replicationLagExceeded(o.maxLag); exceeded {
				fmt.Fprintln(r.out, ui.Danger.Render(fmt.Sprintf(
					"⚠ replication lag %.1fs exceeds --max-lag %s — pausing before next stage (re-run with --resume once standbys catch up)",
					lag.Seconds(), o.maxLag)))
				paused = true
			}
		}
	}
	report.FinishedAt = r.now()
	a, f, p, sk := report.Summary()
	fmt.Fprintf(r.out, "rollout %s: applied %d, failed %d, pending %d, skipped %d\n", name, a, f, p, sk)
	if data, err := json.MarshalIndent(report, "", "  "); err == nil {
		fmt.Fprintln(r.out, ui.Dim.Render("machine-readable report:"))
		fmt.Fprintln(r.out, string(data))
	}
	if f > 0 || p > 0 {
		return fmt.Errorf("rollout incomplete: %d failed, %d pending (re-run with --resume)", f, p)
	}
	return nil
}

// rolloutMode описывает режим применения для отчёта.
func rolloutMode(plan migration.Plan, wrap bool) string {
	switch {
	case plan.NonTransactional:
		return "non-transactional"
	case wrap:
		return "transactional"
	default:
		return "pass-through"
	}
}

// shardsForLabels переводит метки этапа обратно в шарды (в порядке меток).
func shardsForLabels(labels []string, m map[string]cluster.Shard) []cluster.Shard {
	out := make([]cluster.Shard, 0, len(labels))
	for _, l := range labels {
		if s, ok := m[l]; ok {
			out = append(out, s)
		}
	}
	return out
}

// fileBase — имя файла без каталога (без зависимости от filepath на этом пути).
func fileBase(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}
