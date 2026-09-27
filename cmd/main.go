package main

import (
	"context"
	"fmt"
	"time"

	"go.dfds.cloud/bootstrap"
	"go.dfds.cloud/copilot-insights/internal/api"
	"go.dfds.cloud/copilot-insights/internal/auth"
	"go.dfds.cloud/copilot-insights/internal/collector"
	"go.dfds.cloud/copilot-insights/internal/config"
	"go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/metrics"
	"go.dfds.cloud/copilot-insights/internal/store"
	"go.uber.org/zap"
)

const (
	retentionInterval = 24 * time.Hour
	// premiumLegacyInterval only matters until the backfill is complete; later
	// runs request just the members added since.
	premiumLegacyInterval = 24 * time.Hour
)

func main() {
	conf, err := config.Load()
	if err != nil {
		panic(err)
	}

	builder := bootstrap.Builder()
	builder.EnableLogging(conf.LogDebug, conf.LogLevel)
	builder.EnableHttpRouterWithAddr(conf.LogDebug, fmt.Sprintf(":%d", conf.APIPort))
	builder.EnableMetrics()
	manager := builder.Build()
	log := manager.Logger
	defer log.Sync() //nolint:errcheck

	log.Info("copilot-insights launching", zap.String("enterprise", conf.GitHub.Enterprise))

	db, err := store.Open(manager.Context, store.Options{
		Host:         conf.DB.Host,
		Port:         conf.DB.Port,
		Name:         conf.DB.Name,
		User:         conf.DB.User,
		Password:     conf.DB.Password,
		SSLMode:      conf.DB.SSLMode,
		MaxOpenConns: conf.DB.MaxOpenConns,
	})
	if err != nil {
		log.Fatal("failed to open database", zap.Error(err))
	}
	defer db.Close()
	st := store.New(db)

	m := metrics.NewMetrics()
	client, err := github.New(github.Options{
		AppID:         conf.GitHub.AppID,
		AppPrivateKey: []byte(conf.GitHub.AppPrivateKey),
		PAT:           conf.GitHub.PAT,
		APIVersion:    conf.GitHub.APIVersion,
		Timeout:       time.Duration(conf.GitHub.TimeoutMs) * time.Millisecond,
		Logger:        log,
		Metrics:       m,
	})
	if err != nil {
		log.Fatal("failed to create GitHub client", zap.Error(err))
	}

	collectors := collector.New(client, st, collector.Config{
		Enterprise:     conf.GitHub.Enterprise,
		OrgExclude:     conf.GitHub.OrgExclude,
		Concurrency:    conf.GitHub.Concurrency,
		RawDays:        conf.Retention.RawDays,
		ReportLagDays:  conf.Collect.ReportLagDays,
		BackfillDays:   conf.Collect.BackfillDays,
		TeamsEnabled:   conf.Collect.TeamsEnabled,
		BillingLagDays: conf.Collect.BillingLagDays,
	}, log)
	sched := collector.NewScheduler(st, log, m)
	sched.Add(collector.Job{Name: "members", Interval: conf.Collect.MembersInterval, Run: collectors.Members, Prime: true})
	sched.Add(collector.Job{Name: "seats", Interval: conf.Collect.SeatsInterval, Run: collectors.Seats})
	sched.Add(collector.Job{Name: "reports", Interval: conf.Collect.ReportsInterval, Run: collectors.Reports})
	sched.Add(collector.Job{Name: "billing", Interval: conf.Collect.BillingInterval, Run: collectors.Billing})
	sched.Add(collector.Job{Name: "budgets", Interval: conf.Collect.BudgetsInterval, Run: collectors.Budgets})
	if conf.Collect.AICreditEnabled {
		sched.Add(collector.Job{Name: "aicredit", Interval: conf.Collect.AICreditInterval, Run: collectors.AICredits})
	}
	if conf.Collect.PremiumBackfill {
		sched.Add(collector.Job{Name: "premium-legacy", Interval: premiumLegacyInterval, Run: collectors.PremiumLegacy})
	}
	sched.Add(collector.Job{Name: "retention", Interval: retentionInterval, Run: collectors.Retention})

	authMW, adminMW := auth.DisabledMiddleware(), auth.DisabledMiddleware()
	if conf.OIDC.Enabled {
		verifier, err := auth.NewVerifier(manager.Context, conf.OIDC.IssuerURL, conf.OIDC.Audience, conf.OIDC.RequiredRoles, log)
		if err != nil {
			log.Fatal("failed to initialise OIDC verifier", zap.Error(err))
		}
		go verifier.Run(manager.Context, time.Hour)
		onReject := func() { m.AuthRejections.Inc() }
		authMW = verifier.Middleware(onReject)
		adminMW = verifier.RequireRoles(onReject, conf.OIDC.AdminRoles...)
	} else {
		log.Warn("OIDC validation is DISABLED — local dev only")
	}

	api.Configure(manager.HttpRouter, api.Options{
		Ready:      st,
		Store:      st,
		Collectors: sched,
		Enterprise: conf.GitHub.Enterprise,
		AuthMW:     authMW,
		AdminMW:    adminMW,
	})

	applied, err := st.Migrate(manager.Context)
	if err != nil {
		log.Fatal("failed to migrate database", zap.Error(err))
	}
	log.Info("database migrated", zap.Int64s("applied", applied))

	if n, err := st.FailStaleRuns(manager.Context); err != nil {
		log.Warn("could not close stale collector runs", zap.Error(err))
	} else if n > 0 {
		log.Info("closed collector runs interrupted by restart", zap.Int64("runs", n))
	}

	sched.Start(manager.Context, conf.Collect.RunOnStart)
	log.Info("collectors scheduled", zap.Strings("collectors", sched.Names()), zap.Bool("runOnStart", conf.Collect.RunOnStart))

	<-manager.Context.Done()
	sched.Wait()
	if err := manager.HttpServer.Shutdown(context.Background()); err != nil {
		log.Info("HTTP server did not shut down gracefully", zap.Error(err))
	}
	log.Info("copilot-insights shutting down")
}
