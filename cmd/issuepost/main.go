// issuepost は複数のアプリが受け取った案件を 1 か所へ集める台帳である。
//
// このプロセスが持つのは受け口と、その下のデータベースだけ。案件を 1 件受け取り、
// 番号を発行して保存し、番号を返す。**送れなかったら、送ったことにしない。**
package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ishizakahiroshi/doorpost/config"
	"github.com/ishizakahiroshi/doorpost/oplog"

	"github.com/ishizakahiroshi/issuepost/internal/api"
	"github.com/ishizakahiroshi/issuepost/internal/blobs"
	"github.com/ishizakahiroshi/issuepost/internal/store"
)

func main() {
	path := flag.String("config", "config.toml", "設定ファイル（TOML）のパス")
	flag.Parse()

	// 時刻を自分で出さない。常駐は systemd で、journald が 1 行ごとにホストの
	// 現地時刻を前置する。ここで別に時刻を足すと 1 行に 2 つ並び、片方がずれたときに
	// 気づけない。2 つの時計を合わせるより、1 つに減らすほうが後から狂わない。
	logger := oplog.New(os.Stdout)
	std := logger.Std()

	var cfg api.Config
	if err := config.Load(*path, &cfg); err != nil {
		// 設定が通らないまま起動しない。穴の空いた状態で動くより止まるほうがよい。
		std.Fatalf("起動を中止する: %v", err)
	}

	st, err := store.Open(cfg.Database)
	if err != nil {
		std.Fatalf("起動を中止する: %v", err)
	}
	defer func() { _ = st.Close() }()

	// 繋がらないなら起動しない。台帳へ書けないプロセスにできることは 1 つも無く、
	// 受け口だけ開いていると、呼ぶ側から見て「動いているのに毎回失敗する」になる。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = st.Ping(ctx)
	cancel()
	if err != nil {
		std.Fatalf("起動を中止する: %v", err)
	}

	// 添付の置き場。設定に無ければ nil のままで、添付の口だけが動かない。
	// **案件は受け付ける**（設計文書 §6）。
	var files api.Files
	if cfg.Attachments.Configured() {
		dav, err := blobs.New(cfg.Attachments.Config)
		if err != nil {
			std.Fatalf("起動を中止する: %v", err)
		}
		files = dav
	}

	srv := api.New(&cfg, st, files, logger)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutCancel()
		if err := httpSrv.Shutdown(shutCtx); err != nil {
			std.Printf("停止中にエラー: %v", err)
		}
		close(idle)
	}()

	// 起動の 1 行。接続先も鍵も出さない（設定にパスワードが入っている）。
	logger.Print(
		oplog.Str("listen", cfg.Listen),
		oplog.Int("apps", len(cfg.Apps)),
		oplog.Int("kinds", len(cfg.Kinds.Values)),
		oplog.Int("statuses", len(cfg.Statuses.Values)),
		oplog.Bool("attachments", files != nil),
	)

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		std.Fatalf("起動できない: %v", err)
	}
	<-idle
}
