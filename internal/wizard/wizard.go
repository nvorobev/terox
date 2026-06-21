// Package wizard — интерактивное добавление кластера: выводит шаблон хоста
// из первого/последнего имени и пишет его в конфиг.
package wizard

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"

	"terox/internal/cluster"
	"terox/internal/config"
)

// Run показывает форму регистрации, при успехе меняет cfg в памяти и сохраняет
// его. Возвращает имена добавленных service/storage.
func Run(cfg *config.Config) (service, storage string, err error) {
	var (
		firstHost string
		lastHost  string
		portStr   = "6404"
		dbName    = "master"
		user      string
		password  string
		isProd    bool
		sslMode   = "require"
		writeRole string
	)

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("Service name").Placeholder("item").Value(&service).
				Validate(nonEmpty("service")),
			huh.NewInput().Title("Storage name").Placeholder("sharded").Value(&storage).
				Validate(nonEmpty("storage")),
		),
		huh.NewGroup(
			huh.NewInput().Title("First host").
				Placeholder("pgbouncer-...-rs001.db.avito-sd").Value(&firstHost).
				Validate(nonEmpty("first host")),
			huh.NewInput().Title("Last host (same as first for a single DB)").
				Placeholder("pgbouncer-...-rs128.db.avito-sd").Value(&lastHost),
		),
		huh.NewGroup(
			huh.NewInput().Title("Port").Value(&portStr).Validate(validPort),
			huh.NewInput().Title("Database (use {p}/{p1} for per-shard names)").
				Placeholder("master  or  shard_{p}").Value(&dbName),
			huh.NewInput().Title("User").Value(&user).Validate(nonEmpty("user")),
			huh.NewInput().Title("Password").EchoMode(huh.EchoModePassword).Value(&password),
			huh.NewConfirm().Title("Production cluster?").
				Description("Marks the cluster prod: red prompt badge + extra write confirmations.").
				Value(&isProd),
			huh.NewInput().Title("Write role (optional)").
				Description("Writes elevate via 'set local role <role>' (a full-write, non-superuser role). Leave blank for none.").
				Value(&writeRole),
			huh.NewSelect[string]().Title("TLS (sslmode)").
				Description("verify-full: encrypted + server identity verified (recommended for prod).\nrequire: encrypted only (MITM possible). disable: no TLS — local only.").
				Options(
					huh.NewOption("verify-full — recommended for prod", "verify-full"),
					huh.NewOption("require — encrypted, certificate NOT verified", "require"),
					huh.NewOption("disable — no TLS (local/dev only)", "disable"),
				).Value(&sslMode),
		),
	)
	if err := form.Run(); err != nil {
		return "", "", err
	}

	if strings.TrimSpace(lastHost) == "" {
		lastHost = firstHost
	}
	derived, err := cluster.DeriveHostTemplate(strings.TrimSpace(firstHost), strings.TrimSpace(lastHost))
	if err != nil {
		return "", "", err
	}
	port, _ := strconv.Atoi(strings.TrimSpace(portStr))

	st := &config.Storage{
		HostTemplate:  derived.HostTemplate,
		DBTemplate:    strings.TrimSpace(dbName),
		Port:          port,
		User:          strings.TrimSpace(user),
		Password:      password,
		Count:         derived.Count,
		Prod:          isProd,
		SSLMode:       sslMode,
		MigrationRole: strings.TrimSpace(writeRole),
	}

	// Показываем предпросмотр и просим подтверждение перед записью.
	if err := confirmPreview(st, derived); err != nil {
		return "", "", err
	}

	if cfg.Services == nil {
		cfg.Services = map[string]*config.Service{}
	}
	svc := cfg.Services[service]
	if svc == nil {
		svc = &config.Service{Storages: map[string]*config.Storage{}}
		cfg.Services[service] = svc
	}
	if svc.Storages == nil {
		svc.Storages = map[string]*config.Storage{}
	}
	svc.Storages[storage] = st

	if err := cfg.Save(); err != nil {
		return "", "", fmt.Errorf("save config: %w", err)
	}
	return service, storage, nil
}

// confirmPreview показывает первый/последний шарды и просит подтверждение.
func confirmPreview(st *config.Storage, d cluster.DerivedTemplate) error {
	shards, err := cluster.Expand(st)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Template: %s\n", st.HostTemplate)
	fmt.Fprintf(&b, "Shards:   %d\n\n", len(shards))
	preview := func(s cluster.Shard) {
		fmt.Fprintf(&b, "  %-8s %s:%d/%s\n", s.Label, s.Host, s.Port, s.DB)
	}
	switch {
	case len(shards) <= 4:
		for _, s := range shards {
			preview(s)
		}
	default:
		preview(shards[0])
		preview(shards[1])
		b.WriteString("  ...\n")
		preview(shards[len(shards)-2])
		preview(shards[len(shards)-1])
	}
	// Шаблон {p1} нумерует с 1, поэтому диапазон с другим началом
	// (например rs050..rs060) дал бы неверные хосты (rs001..rs011) —
	// возможно из другого окружения. Отказываемся регистрировать; такой
	// кластер добавляется правкой config.yaml вручную.
	if d.Start != 1 {
		return fmt.Errorf("hosts start at %d, but the wizard's template numbers from 1 — registering this would generate the wrong hosts (starting at 1, not %d).\nedit config.yaml manually for a non-1-based range, or use hosts that start at 1", d.Start, d.Start)
	}

	confirmed := false
	err = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Register this cluster?").Description(b.String()).Value(&confirmed),
	)).Run()
	if err != nil {
		return err
	}
	if !confirmed {
		return fmt.Errorf("cancelled")
	}
	return nil
}

func nonEmpty(field string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s is required", field)
		}
		return nil
	}
}

func validPort(s string) error {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 || n > 65535 {
		return fmt.Errorf("invalid port")
	}
	return nil
}
