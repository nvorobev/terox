package repl

import (
	"fmt"
	"io"
	"sync"
	"testing"

	"terox/internal/complete"
)

// TestCatalogStatusNoRace гоняет вывод статуса \completion одновременно с
// фоновой мутацией того же *Catalog (как loadColumnsAsync под catalogMu).
// Запускать с -race.
func TestCatalogStatusNoRace(t *testing.T) {
	cat := &complete.Catalog{Shards: 2, SearchPath: []string{"public"}, Coverage: map[string]int{}}
	r := &REPL{out: io.Discard, catalog: cat}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// Имитирует фоновый загрузчик колонок: добавляет колонки и coverage
			// под catalogMu.
			r.catalogMu.Lock()
			cat.SetColumns("public", fmt.Sprintf("t%d", i), []complete.Column{
				{Schema: "public", Relation: fmt.Sprintf("t%d", i), Name: "c"}})
			mergeCoverage(cat, map[string]int{fmt.Sprintf("col:public.t%d.c", i): 1})
			r.catalogMu.Unlock()
		}
	}()

	for i := 0; i < 2000; i++ {
		_ = r.catalogStatus() // снимок под блокировкой
		r.doCompletion(nil)   // путь вывода статуса \completion
	}
	close(stop)
	wg.Wait()
}
