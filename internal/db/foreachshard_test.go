package db

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"terox/internal/cluster"
)

func mkShards(n int) []cluster.Shard {
	shards := make([]cluster.Shard, n)
	for i := range shards {
		shards[i] = cluster.Shard{Position: i, Label: fmt.Sprintf("s%d", i)}
	}
	return shards
}

// TestForEachShardStopOnError проверяет, что при concurrency 1 и stopOnError
// первая ошибка прерывает остальные шарды, и при этом у каждого шарда есть результат.
func TestForEachShardStopOnError(t *testing.T) {
	m := NewManager()
	defer m.Close()
	shards := mkShards(10)
	var ran int64
	results := m.ForEachShard(context.Background(), shards, 1, 0, true,
		func(ctx context.Context, s cluster.Shard) (int64, error) {
			atomic.AddInt64(&ran, 1)
			return 0, fmt.Errorf("boom on %s", s.Label)
		})
	if len(results) != len(shards) {
		t.Fatalf("want %d results, got %d", len(shards), len(results))
	}
	if got := atomic.LoadInt64(&ran); got != 1 {
		t.Errorf("stop-on-error with concurrency 1 should run exactly 1 shard, ran %d", got)
	}
	// Шарды после первого несут ошибку отмены, а не результат собственного запуска.
	if results[0].Err == nil {
		t.Error("first shard should carry its boom error")
	}
	for _, r := range results[1:] {
		if r.Err == nil {
			t.Errorf("shard %s should have been short-circuited", r.Shard.Label)
		}
	}
}

// TestForEachShardContinueRunsAll проверяет: при stopOnError=false выполняются
// все шарды, даже если часть из них падает (режим сбора статуса, например \ping).
func TestForEachShardContinueRunsAll(t *testing.T) {
	m := NewManager()
	defer m.Close()
	shards := mkShards(8)
	var ran int64
	m.ForEachShard(context.Background(), shards, 4, 0, false,
		func(ctx context.Context, s cluster.Shard) (int64, error) {
			atomic.AddInt64(&ran, 1)
			if s.Position == 0 {
				return 0, fmt.Errorf("boom")
			}
			return 0, nil
		})
	if got := atomic.LoadInt64(&ran); got != int64(len(shards)) {
		t.Errorf("continue mode should run all %d shards, ran %d", len(shards), got)
	}
}

// TestForEachShardNoSemaphoreLeak проверяет, что отменённый контекст не утекает
// слотами семафора: при заранее отменённом контексте вызов быстро возвращается с
// прерванными шардами и не зависает, а повторный вызов на том же Manager работает
// штатно (слоты не исчерпаны).
func TestForEachShardNoSemaphoreLeak(t *testing.T) {
	m := NewManager()
	defer m.Close()
	shards := mkShards(20)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // отменён ещё до старта цикла

	done := make(chan []ExecResult, 1)
	go func() {
		done <- m.ForEachShard(ctx, shards, 4, 0, false,
			func(ctx context.Context, s cluster.Shard) (int64, error) { return 0, nil })
	}()
	select {
	case res := <-done:
		if len(res) != len(shards) {
			t.Fatalf("want %d results, got %d", len(shards), len(res))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ForEachShard deadlocked on a pre-cancelled context (semaphore leak)")
	}

	// Слоты не исчерпаны: новый вызов с живым контекстом выполняет все шарды.
	var ran int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.ForEachShard(context.Background(), mkShards(6), 4, 0, false,
			func(ctx context.Context, s cluster.Shard) (int64, error) {
				atomic.AddInt64(&ran, 1)
				return 0, nil
			})
	}()
	wg.Wait()
	if got := atomic.LoadInt64(&ran); got != 6 {
		t.Errorf("second call should run all 6 shards (no leaked slots), ran %d", got)
	}
}
