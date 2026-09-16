// main.go lobsterai2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"lobsterai2api/internal/auth"
	"lobsterai2api/internal/console"
	"lobsterai2api/internal/pool"
	"lobsterai2api/internal/reqlog"
	"lobsterai2api/internal/scheduler"
	"lobsterai2api/internal/server"
	"lobsterai2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	p := pool.New(cfg.StateFile)
	for _, a := range auths {
		p.Add(a)
	}

	up := upstream.New()
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second

	// 请求日志：内存环形缓冲（500 条）+ data/requests.jsonl 持久化
	reqLogFile := filepath.Join(cfg.AuthDir, "..", "data", "requests.jsonl")
	rl := reqlog.New(reqLogFile, 500)

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
	})

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		HardCooldown: cfg.HardCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
		ReqLog:       rl,
	})

	// 内置 Web 控制台（/console）：添加账号 + 数据展示
	portal := os.Getenv("LB2A_LOGIN_PORTAL")
	if portal == "" {
		portal = "https://lobsterai.youdao.com"
	}
	con, err := console.New(console.Config{
		Pool:      p,
		Upstream:  up,
		APIKey:    cfg.APIKey,
		AuthDir:   cfg.AuthDir,
		PortalURL: portal,
		ReqLog:    rl,
	})
	if err != nil {
		log.Fatalf("console: %v", err)
	}
	root := http.NewServeMux()
	root.Handle("/console", con)
	root.Handle("/console/", con)
	root.Handle("/", h)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	// 启动后延迟拉一次积分（立即刷新 pool.credits，不用等整点）
	go func() {
		time.Sleep(5 * time.Second)
		sch.RunCheckinNow()
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           root,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("lobsterai2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
