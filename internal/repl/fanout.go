package repl

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"time"

	"terox/internal/cluster"
	"terox/internal/db"
	"terox/internal/ui"
)

// fanout выполняет sql по шардам с полной материализацией (без лимита строк на шард).
func (r *REPL) fanout(shards []cluster.Shard, sql string, readOnly bool, perShardTimeout time.Duration) []db.ShardResult {
	return r.fanoutLimit(shards, sql, readOnly, perShardTimeout, 0)
}

// fanoutLimit выполняет sql по шардам; в интерактивном режиме при многих шардах
// показывает живую строку прогресса "k/N", а Ctrl-C отменяет всю группу
// (текущие запросы прерываются через контекст). limit ограничивает число строк
// на шард (0 = без лимита). Возвращает результаты по шардам; у отменённых шардов
// ошибка контекста.
func (r *REPL) fanoutLimit(shards []cluster.Shard, sql string, readOnly bool, perShardTimeout time.Duration, limit int) []db.ShardResult {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	var mu sync.Mutex
	cancelled := false
	go func() {
		select {
		case <-sig:
			mu.Lock()
			cancelled = true
			mu.Unlock()
			cancel()
		case <-ctx.Done():
		}
	}()

	total := len(shards)
	showProgress := ui.Enabled && total > 3
	var onDone func(done, total int, sr db.ShardResult)
	if showProgress {
		var pmu sync.Mutex
		fails, last := 0, 0
		onDone = func(done, total int, sr db.ShardResult) {
			pmu.Lock()
			defer pmu.Unlock()
			if sr.Err != nil {
				fails++
			}
			if done > last {
				last = done
			}
			fmt.Fprintf(r.out, "\r\x1b[K  running… %d/%d shards (%d failed) — Ctrl-C to cancel", last, total, fails)
		}
	}

	results := r.mgr.FanoutProgress(ctx, shards, sql, readOnly, r.cfg.Concurrency(len(shards)), perShardTimeout, limit, onDone)

	if showProgress {
		fmt.Fprint(r.out, "\r\x1b[K") // очистить строку прогресса
	}
	mu.Lock()
	wasCancelled := cancelled
	mu.Unlock()
	if wasCancelled {
		ok := 0
		for _, sr := range results {
			if sr.Err == nil {
				ok++
			}
		}
		fmt.Fprintf(r.out, "%s cancelled — %d/%d shards completed before Ctrl-C\n", ui.Danger.Render("⚠"), ok, total)
	}
	return results
}

// interruptible возвращает контекст, отменяемый по первому Ctrl-C, и функцию его
// освобождения. Используется циклическими fan-out (\ping, \watch), чтобы Ctrl-C
// прерывал текущую работу по шардам.
func interruptible() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		select {
		case <-sig:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(sig)
	}()
	return ctx, cancel
}
