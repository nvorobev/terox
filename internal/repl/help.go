package repl

import (
	"fmt"
	"sort"
	"strings"

	"terox/internal/ui"
)

// helpEntry описывает одну мета-команду для справки.
type helpEntry struct {
	names    []string // основное имя + алиасы (без ведущего обратного слэша)
	category string
	syntax   string
	summary  string
	examples []string
	risk     string // "" нет, иначе короткое предупреждение-бейдж
}

// helpCategories — порядок вывода категорий.
var helpCategories = []string{
	"Navigation", "Modes & input", "Diagnostics", "Data", "Plans",
	"Files & migrations", "Saved queries", "History & misc",
}

var helpEntries = []helpEntry{
	{[]string{"use"}, "Navigation", "\\use [service]", "Switch service (interactive service→storage→shard menu; Esc steps back).", []string{"\\use", "\\use item"}, ""},
	{[]string{"c", "connect"}, "Navigation", "\\c <storage> [selector]", "Switch storage within the current service. selector = label, \"all\", or \"0,1,3..7\".", []string{"\\c prod_item all", "\\c cold 0,1,2"}, ""},
	{[]string{"shard", "s"}, "Navigation", "\\shard [selector]", "Re-target shards in the current storage (menu if omitted). The fast path after finding data on one shard.", []string{"\\s rs042", "\\s 0,1,5", "\\shard all"}, ""},
	{[]string{"shards"}, "Navigation", "\\shards", "List the storage's shards and which are currently targeted.", nil, ""},
	{[]string{"l", "list"}, "Navigation", "\\l", "List services and storages.", nil, ""},
	{[]string{"add"}, "Navigation", "\\add", "Register a new cluster via the wizard (first/last host → template + range).", nil, ""},

	{[]string{"write"}, "Modes & input", "\\write on|off", "Toggle write mode. Read-only by default; writes need it on, plus confirmation. Resets to read-only when you switch storage/service (a per-context lease).", []string{"\\write on"}, "enables destructive statements on prod"},
	{[]string{"write_approve"}, "Modes & input", "\\write_approve [on|off]", "Toggle the write confirmation prompt (on by default). With it off, writes run without asking.", []string{"\\write_approve off"}, "disables the confirmation barrier before writes"},
	{[]string{"timeout"}, "Modes & input", "\\timeout [value|off]", "Set statement_timeout (server-side, bounds reads and migrations). Shown as st= in the prompt.", []string{"\\timeout 500ms", "\\timeout 2min", "\\timeout off"}, ""},
	{[]string{"maxrows"}, "Modes & input", "\\maxrows [N|unlimited]", "Row display limit per query. Rejects invalid/negative input.", []string{"\\maxrows 50", "\\maxrows unlimited"}, ""},
	{[]string{"timing"}, "Modes & input", "\\timing [on|off]", "Show/hide query duration in the result footer (on by default).", nil, ""},
	{[]string{"x"}, "Modes & input", "\\x", "Toggle expanded (one-field-per-line) display for wide rows.", nil, ""},
	{[]string{"impact"}, "Modes & input", "\\impact [on|off]", "Toggle the pre-write affected-row preview (off by default).", nil, ""},
	{[]string{"suggest"}, "Modes & input", "\\suggest [on|off]", "Toggle the inline ghost autosuggestion (on by default).", nil, ""},
	{[]string{"editor"}, "Modes & input", "\\editor [tea|readline]", "Pick the line editor (menu if no arg): readline (classic) or tea (live completion dropdown). Saved to config; effective on the next input line.", []string{"\\editor", "\\editor tea"}, ""},
	{[]string{"layout"}, "Modes & input", "\\layout on|off", "Auto-convert Cyrillic input to Latin keyboard positions (ЙЦУКЕН→QWERTY) so commands/SQL typed on a Russian layout still work. On by default; kept verbatim inside string literals. Saved to config.", []string{"\\layout off"}, ""},
	{[]string{"e", "edit"}, "Modes & input", "\\e", "Edit a (large/multi-line) query in $VISUAL/$EDITOR (or vi), pre-filled with the last query so you can tweak and re-run it (psql-style). Save & quit to run it (:wq in vim, Ctrl-O then Ctrl-X in nano); leave the buffer empty to cancel.", nil, ""},

	{[]string{"ping"}, "Diagnostics", "\\ping", "Connectivity and latency of every targeted shard.", nil, ""},
	{[]string{"doctor"}, "Diagnostics", "\\doctor [--all]", "Health check (connections, locks, bloat, invalid indexes, replication slots, wraparound...). --all aggregates across every shard.", []string{"\\doctor", "\\doctor --all"}, ""},
	{[]string{"heal"}, "Diagnostics", "\\heal [--apply]", "Find invalid indexes (leftovers of a failed CREATE INDEX CONCURRENTLY) on the targeted shards and print a ready DROP INDEX CONCURRENTLY for each. Read-only without --apply; --apply drops them per shard (needs \\write on, asks for confirmation, and a stricter 'drop' barrier on prod).", []string{"\\heal", "\\heal --apply"}, "--apply drops indexes (needs \\write on)"},
	{[]string{"diff"}, "Diagnostics", "\\diff <table>", "Compare a table across the targeted shards (schema drift): columns (type/null/default/identity/generated/collation), indexes (incl. INVALID/NOT READY), constraints (incl. NOT VALID), triggers, RLS + policies, partition bound, and view/matview definition. Lists which dimensions differ.", []string{"\\diff items"}, ""},
	{[]string{"compare"}, "Diagnostics", "\\compare <service/storage>", "Diff schema, indexes, extension versions and config vs another storage (why it behaves differently there).", []string{"\\compare cold/prod"}, ""},
	{[]string{"completion"}, "Diagnostics", "\\completion [status|reload]", "Show the autocomplete catalog state (flags partial/forbidden/timeout segments and per-shard coverage), or reload it.", nil, ""},
	{[]string{"activity"}, "Diagnostics", "\\activity [--all] [--raw]", "Live backends per shard from pg_stat_activity (state, wait events, xact age, query). --all includes idle sessions; query literals are masked by default, --raw shows them verbatim.", []string{"\\activity", "\\activity --all"}, ""},
	{[]string{"blockers"}, "Diagnostics", "\\blockers [--raw]", "Blocked backends and who blocks them (pg_blocking_pids), per shard, with a by_autovacuum flag. Literals masked unless --raw.", nil, ""},
	{[]string{"locks"}, "Diagnostics", "\\locks", "Lock summary per shard (mode/locktype/granted counts).", nil, ""},
	{[]string{"longtx"}, "Diagnostics", "\\longtx [duration] [--raw]", "Transactions (incl. idle-in-transaction) older than a threshold (default 1m). Literals masked unless --raw.", []string{"\\longtx", "\\longtx 30s"}, ""},
	{[]string{"sizes"}, "Diagnostics", "\\sizes [N]", "Top-N user tables by total size (heap + indexes + TOAST) with dead-tuple % (bloat), per shard. Default 20. The leading shard column surfaces cross-shard size skew.", []string{"\\sizes", "\\sizes 10"}, ""},
	{[]string{"statements", "workload"}, "Diagnostics", "\\statements [N|snapshot|diff] [--mean|--calls|--rows|--max] [--user U] [--db D] [--queryid Q] [--skew]", "Top-N queries from pg_stat_statements (needs the extension; a friendly hint if absent), per shard. Columns are version-gated (max/stddev/WAL on 13+) with db/role identity. --skew aggregates a queryid across shards to surface imbalance. 'snapshot' captures the current workload and 'diff' compares against it to surface regressions (queryid is server-local). Default top 20 by total time.", []string{"\\statements", "\\statements 10 --mean", "\\statements --skew", "\\statements snapshot", "\\statements diff"}, ""},
	{[]string{"copy"}, "Data", "\\copy <table|(query)> to <file> [csv|text|tsv] | \\copy <table> from <file> [csv|tsv]", "Client-side COPY: export rows to a local file, or load a local file into a table (single shard; from requires \\write on). Server never sees the path; TO PROGRAM/server files are impossible.", []string{"\\copy users to users.csv", "\\copy (select * from users where active) to active.csv", "\\copy users from users.csv csv"}, "from loads/writes data"},
	{[]string{"advise"}, "Plans", "\\advise <query>", "Index advisor: EXPLAINs the query (does not run it), finds filtered seq scans, and suggests an index (with rollback) where none leads with the filter column. Heuristic — verify before creating.", []string{"\\advise select * from users where email = 'x'"}, ""},
	{[]string{"lint"}, "Plans", "\\lint <sql>", "Static pre-execution diagnostics (no DB): unqualified write, LIMIT without ORDER BY, NOT IN with nullable, SELECT *, likely Cartesian product, multiple statements. Heuristics with confidence — not a substitute for EXPLAIN.", []string{"\\lint select * from a, b", "\\lint delete from t"}, ""},
	{[]string{"cancel"}, "Diagnostics", "\\cancel <pid>", "Cancel the running query of a backend (pg_cancel_backend) on the single selected shard.", []string{"\\cancel 12345"}, ""},
	{[]string{"terminate"}, "Diagnostics", "\\terminate <pid>", "Terminate a backend (pg_terminate_backend) on the single selected shard. Asks for confirmation; refuses terox's own backends.", []string{"\\terminate 12345"}, "terminates the connection"},

	{[]string{"count"}, "Data", "\\count <table> [where]", "Per-shard counts plus a cluster TOTAL.", []string{"\\count items", "\\count items status='new'"}, ""},
	{[]string{"locate", "find"}, "Data", "\\locate <table> [where]", "Find which shards hold matching rows (auto-jumps when exactly one matches).", []string{"\\locate items item_id=200"}, ""},
	{[]string{"dt"}, "Data", "\\dt", "List user tables.", nil, ""},
	{[]string{"dn"}, "Data", "\\dn", "List user schemas.", nil, ""},
	{[]string{"di"}, "Data", "\\di", "List indexes of user tables, with their size.", nil, ""},
	{[]string{"d"}, "Data", "\\d [table]", "Without an argument, list tables (per shard). With a table, describe it on the first shard: columns, indexes, foreign keys, what references it, check constraints and size (note: first-shard sample — use \\diff for cross-shard drift).", []string{"\\d", "\\d items"}, ""},
	{[]string{"export"}, "Data", "\\export csv|json <file>", "Write the last result to a file.", []string{"\\export csv out.csv", "\\export json rows.json"}, ""},
	{[]string{"watch"}, "Data", "\\watch [interval] <query | \\diag-command>", "Re-run a read query OR a diagnostic command (\\activity, \\blockers, \\locks, \\longtx, \\statements) on an interval (Ctrl-C to stop). Interval accepts ms/s/m/h (e.g. 500ms, 5s, 2m) or bare seconds; must be positive; default 2s.", []string{"\\watch 500ms select count(*) from items", "\\watch 2s \\activity"}, ""},
	{[]string{"g", "gx"}, "Data", "\\g [selector] | \\gx [selector]", "Re-run the last query; \\gx shows it in expanded display. An optional shard selector re-runs it on just those shards (the rest of the context is untouched). A write would be re-gated, not repeated silently.", []string{"\\g", "\\g rs042", "\\gx 0,1"}, ""},
	{[]string{"grep"}, "Data", "\\grep [-v] <pattern>", "Filter the last result in memory by a case-insensitive substring in any cell (no re-query). -v keeps non-matching rows. A footer shows how many of the fetched rows matched.", []string{"\\grep failed", "\\grep -v ok"}, ""},

	{[]string{"explain"}, "Plans", "\\explain [analyze] [--memory|--serialize|--generic-plan] [--all|--first|--shard L|--sample N|--outliers] <query>", "Human-readable plan diagnosis with a worst-cardinality-estimate drill-down. Across shards it groups by structural fingerprint and flags outliers. analyze executes the query (reads only, and warns that volatile side effects are not undone by ROLLBACK); on many shards it defaults to a single-shard canary. --memory/--serialize need PG17, --generic-plan needs PG16 (no analyze). Subcommands: save <name> <q>, compare <name>, diff a.json b.json, -f plan.json.", []string{"\\explain analyze --first select ...", "\\explain --generic-plan select * from t where id = $1", "\\explain save base analyze select ...", "\\explain diff before.json after.json"}, "analyze EXECUTES the query"},

	{[]string{"migrate", "mig"}, "Files & migrations", "\\migrate [--allowed] [--check] [--canary|--batch N|--resume] [--force] <file.sql>", "Run a migration body; terox wraps it in a single transaction with set local role + statement_timeout and sends it as ONE exec. DRY-RUN BY DEFAULT (prints the exact exec + rollout plan + a migration-aware lint); pass --allowed to apply. --check runs only the static lint (no DB): flags ADD COLUMN NOT NULL without DEFAULT, CREATE INDEX without CONCURRENTLY, ADD FK/CHECK without NOT VALID, table-rewriting ALTER TYPE, DROP without IF EXISTS. Rollout: --canary applies one shard first, --batch N applies in batches with a barrier, --resume re-applies only shards not yet in the ledger; a checksum mismatch is BLOCKED unless --force. Emits a machine-readable report.", []string{"\\migrate m.sql", "\\migrate --check m.sql", "\\migrate --allowed --canary m.sql"}, "with --allowed writes to every targeted shard"},
	{[]string{"i", "include"}, "Files & migrations", "\\i [--allowed] <file.sql>", "Run a SQL file verbatim as one exec (you own begin/commit and set role). For experts. DRY-RUN BY DEFAULT; pass --allowed to actually run it.", []string{"\\i custom.sql", "\\i --allowed custom.sql"}, "raw pass-through, no safety wrapper"},

	{[]string{"save"}, "Saved queries", "\\save <name> [sql]", "Save a named query (the last query if sql is omitted). Use :name placeholders for parameters.", []string{"\\save byid select * from items where item_id = :id"}, ""},
	{[]string{"run"}, "Saved queries", "\\run <name>", "Run a saved query, prompting for any :name parameters (numbers inserted raw, else as quoted literals) and previewing the bound SQL.", []string{"\\run byid"}, ""},
	{[]string{"queries"}, "Saved queries", "\\queries", "List saved queries.", nil, ""},
	{[]string{"unsave"}, "Saved queries", "\\unsave <name>", "Delete a saved query.", nil, ""},

	{[]string{"h", "history"}, "History & misc", "\\h", "History hint (Up/Down to browse, Ctrl-R to search).", nil, ""},
	{[]string{"help", "?"}, "History & misc", "\\help [command]", "This help. With a command, shows its syntax, examples and risks.", []string{"\\help explain", "\\help write"}, ""},
	{[]string{"q", "quit"}, "History & misc", "\\q", "Quit.", nil, ""},
}

func lookupHelp(name string) *helpEntry {
	name = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), "\\")
	for i := range helpEntries {
		for _, n := range helpEntries[i].names {
			if n == name {
				return &helpEntries[i]
			}
		}
	}
	return nil
}

// searchHelp возвращает команды, у которых ключевое слово встречается в имени,
// синтаксисе или назначении — поиск по справке (\help lock → \locks, \blockers, \longtx).
func searchHelp(q string) []*helpEntry {
	q = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(q), "\\"))
	if q == "" {
		return nil
	}
	var out []*helpEntry
	for i := range helpEntries {
		e := &helpEntries[i]
		hay := strings.ToLower(e.summary + " " + e.syntax + " " + strings.Join(e.names, " "))
		if strings.Contains(hay, q) {
			out = append(out, e)
		}
	}
	return out
}

// printHelp выводит список команд по категориям; с аргументом — подробную
// справку по одной команде.
func (r *REPL) printHelp(args []string) {
	if len(args) > 0 {
		if e := lookupHelp(args[0]); e != nil {
			r.printHelpEntry(e)
			return
		}
		// Точного совпадения нет — ищем по намерению (подстрока в имени/назначении):
		// \help lock → \locks, \blockers, \longtx.
		if matches := searchHelp(args[0]); len(matches) > 0 {
			fmt.Fprintf(r.out, "no command \\%s; related:\n", strings.TrimPrefix(args[0], "\\"))
			for _, e := range matches {
				fmt.Fprintf(r.out, "  %-14s %s\n", "\\"+e.names[0], e.summary)
			}
			return
		}
		fmt.Fprintf(r.out, "no help for %q — try \\help for the command list\n", args[0])
		return
	}

	byCat := map[string][]helpEntry{}
	for _, e := range helpEntries {
		byCat[e.category] = append(byCat[e.category], e)
	}
	for _, cat := range helpCategories {
		es := byCat[cat]
		if len(es) == 0 {
			continue
		}
		fmt.Fprintf(r.out, "\n%s\n", ui.Service.Render(cat))
		sort.SliceStable(es, func(i, j int) bool { return es[i].names[0] < es[j].names[0] })
		for _, e := range es {
			cmd := "\\" + e.names[0]
			risk := ""
			if e.risk != "" {
				risk = "  " + ui.Danger.Render("⚠")
			}
			fmt.Fprintf(r.out, "  %-14s %s%s\n", cmd, e.summary, risk)
		}
	}
	fmt.Fprintf(r.out, "\n%s  Writes run under \"set local role _fa\" + statement_timeout in ONE transaction/exec (pgbouncer txn pooling);\n",
		ui.Dim.Render("note:"))
	fmt.Fprintln(r.out, "       CONCURRENTLY/VACUUM run as separate execs. Target many shards → merged table with a")
	fmt.Fprintln(r.out, "       \"shard\" column. \\help <command> for syntax, examples and risks.")
}

func (r *REPL) printHelpEntry(e *helpEntry) {
	names := make([]string, len(e.names))
	for i, n := range e.names {
		names[i] = "\\" + n
	}
	fmt.Fprintf(r.out, "%s   %s\n", ui.Service.Render(strings.Join(names, ", ")), ui.Dim.Render("["+e.category+"]"))
	fmt.Fprintf(r.out, "  syntax: %s\n", e.syntax)
	fmt.Fprintf(r.out, "  %s\n", e.summary)
	if e.risk != "" {
		fmt.Fprintf(r.out, "  %s %s\n", ui.Danger.Render("⚠ risk:"), e.risk)
	}
	for i, ex := range e.examples {
		if i == 0 {
			fmt.Fprintln(r.out, "  examples:")
		}
		fmt.Fprintf(r.out, "    %s\n", ex)
	}
}
