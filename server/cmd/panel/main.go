// SPDX-License-Identifier: AGPL-3.0-or-later

// Command panel 是控制面的单一二进制（spec/01 1.1）：
//
//	panel api|gateway|worker|all   启动角色（all 在同一进程中启动全部角色）
//	panel migrate [status]         执行数据库迁移或查看状态（spec/40 DEP-12）
//	panel admin create --email E   创建首个超级管理员（spec/10 AUTH-21）
//	panel version                  输出版本与提交
//
// 配置文件由 --config 或环境变量 PANEL_CONFIG 指定，环境变量可覆盖其中的字段（internal/config）。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/valkey-io/valkey-go"
	"golang.org/x/term"

	"github.com/akari-project/panel/server/internal/admin"
	"github.com/akari-project/panel/server/internal/app"
	"github.com/akari-project/panel/server/internal/auth/token"
	"github.com/akari-project/panel/server/internal/buildinfo"
	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/config"
	"github.com/akari-project/panel/server/internal/db"
	"github.com/akari-project/panel/server/internal/kv"
	"github.com/akari-project/panel/server/internal/logging"
	"github.com/akari-project/panel/server/internal/password"
	"github.com/akari-project/panel/server/internal/secretbox"
	"github.com/akari-project/panel/server/internal/webui"
)

const usage = `usage: panel <command> [flags]

commands:
  api | gateway | worker | all   run control-plane roles
  migrate [status]               apply database migrations, or show their status
  admin create --email <email>   create the first superadmin
  version                        print version and commit

flags:
  --config <file>                YAML configuration (default: $PANEL_CONFIG)
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "panel:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stderr, usage)
		if len(args) == 0 {
			return errors.New("missing command")
		}
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Fprintf(stdout, "panel %s (commit %s, ui %t)\n", buildinfo.Version, orNone(buildinfo.Commit), webui.Embedded() != nil)
		return nil
	case "migrate":
		return runMigrate(ctx, rest, stdout, stderr)
	case "admin":
		if len(rest) == 0 || rest[0] != "create" {
			return errors.New("usage: panel admin create --email <email> [--password-stdin]")
		}
		return runAdminCreate(ctx, rest[1:], stdin, stdout, stderr)
	}
	mode, err := app.ParseMode(cmd)
	if err != nil {
		fmt.Fprint(stderr, usage)
		return err
	}
	return runRoles(ctx, mode, rest, stderr)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// loadCommon 解析 --config、加载并校验配置、建立日志。
func loadCommon(fs *flag.FlagSet, args []string, stderr io.Writer) (config.Config, *slog.Logger, error) {
	cfgPath := fs.String("config", os.Getenv("PANEL_CONFIG"), "YAML configuration file")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, nil, err
	}
	if fs.NArg() > 0 && fs.Name() != "migrate" {
		return config.Config{}, nil, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	cfg, err := config.Load(*cfgPath, os.LookupEnv)
	if err != nil {
		return config.Config{}, nil, err
	}
	log, err := logging.New(stderr, cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return config.Config{}, nil, err
	}
	return cfg, log, nil
}

func runRoles(ctx context.Context, mode app.Mode, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet(string(mode), flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg, log, err := loadCommon(fs, args, stderr)
	if err != nil {
		return err
	}
	// 先校验嵌入产物，不一致时在连接数据库之前拒绝启动（DEP-01）。
	assets := webui.Embedded()
	if assets != nil {
		if err := webui.Verify(assets, buildinfo.Commit); err != nil {
			return err
		}
	}
	pool, err := db.Open(ctx, cfg.Database.URL, cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	// api 角色需要 Valkey 与访问令牌签名密钥，api 与 worker 需要主密钥；只启动 gateway 时都不需要。
	var (
		kvc    valkey.Client
		tokens *token.Keyring
		keys   *secretbox.Keyring
	)
	if mode != app.ModeGateway {
		if keys, err = secretbox.ParseKeyring(cfg.Crypto.MasterKey, cfg.Crypto.PreviousMasterKey); err != nil {
			return err
		}
	}
	if mode == app.ModeAPI || mode == app.ModeAll {
		if cfg.Valkey.URL == "" {
			return errors.New("valkey.url is required for the api role (or set PANEL_VALKEY_URL)")
		}
		if tokens, err = token.NewKeyring(clock.Real{}, cfg.Crypto.TokenKey, cfg.Crypto.PreviousTokenKey); err != nil {
			return err
		}
		if cfg.Crypto.PreviousTokenKey != "" {
			// 旧密钥只在轮换后的 30 分钟内需要（AUTH-06），提醒运维按时移除。
			log.Warn("PANEL_TOKEN_KEY_PREVIOUS is set; remove it 30 minutes after rotating PANEL_TOKEN_KEY")
		}
		if kvc, err = kv.Open(ctx, cfg.Valkey.URL); err != nil {
			return err
		}
		defer kvc.Close()
	}
	return app.Run(ctx, app.Deps{
		Config:  cfg,
		Log:     log,
		Clock:   clock.Real{},
		Pool:    pool,
		KV:      kvc,
		Tokens:  tokens,
		Keys:    keys,
		Assets:  assets,
		Version: buildinfo.Version,
		Commit:  buildinfo.Commit,
	}, mode)
}

func runMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg, log, err := loadCommon(fs, args, stderr)
	if err != nil {
		return err
	}
	sub := fs.Arg(0)
	if sub != "" && sub != "status" {
		return fmt.Errorf("usage: panel migrate [status]")
	}
	pool, err := db.Open(ctx, cfg.Database.URL, cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	if sub == "status" {
		st, err := db.Status(ctx, pool)
		if err != nil {
			return err
		}
		for _, s := range st {
			state := "pending"
			if s.IsApplied {
				state = "applied"
			}
			fmt.Fprintf(stdout, "%05d  %-8s %s\n", s.Version, state, s.Source)
		}
		return nil
	}
	v, err := db.Migrate(ctx, pool, log)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "database at version %d\n", v)
	return nil
}

func runAdminCreate(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("admin create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	email := fs.String("email", "", "email address of the first superadmin")
	pwStdin := fs.Bool("password-stdin", false, "read the password from standard input instead of prompting")
	cfg, log, err := loadCommon(fs, args, stderr)
	if err != nil {
		return err
	}
	if *email == "" {
		return errors.New("--email is required")
	}
	keys, err := secretbox.ParseKeyring(cfg.Crypto.MasterKey, cfg.Crypto.PreviousMasterKey)
	if err != nil {
		return err
	}
	pw, err := readPassword(stdin, stderr, *pwStdin)
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.Database.URL, cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	if _, _, err := db.CheckVersion(ctx, pool); err != nil {
		return err
	}
	c := &admin.Creator{Pool: pool, Clock: clock.Real{}, Keys: keys, Password: password.DefaultParams}
	res, err := c.Create(ctx, *email, pw)
	if err != nil {
		return err
	}
	log.Info("superadmin created", logging.KeyAccountID, res.AccountID.String())
	fmt.Fprintf(stdout, "superadmin created: account %s\nTOTP enrollment is required at first sign-in (spec/10 AUTH-21).\n", res.AccountID)
	return nil
}

// readPassword 在终端上提示输入两次（不回显）；否则从标准输入读取一行。
func readPassword(stdin io.Reader, stderr io.Writer, fromStdin bool) (string, error) {
	if f, ok := stdin.(*os.File); ok && !fromStdin && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(stderr, "Password: ")
		a, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		fmt.Fprint(stderr, "Repeat password: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("passwords do not match")
		}
		return string(a), password.CheckLength(string(a))
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	pw := strings.TrimRight(line, "\r\n")
	return pw, password.CheckLength(pw)
}
