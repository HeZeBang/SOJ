package main

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/mrhaoxx/SOJ/deploy"
	"github.com/mrhaoxx/SOJ/file_transfer"
	"github.com/mrhaoxx/SOJ/judge"
	"github.com/mrhaoxx/SOJ/types"
	"github.com/mrhaoxx/SOJ/ui"

	ssh "github.com/gliderlabs/ssh"
	"github.com/logrusorgru/aurora/v4"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	gossh "golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	// 读取配置
	var cfg types.Config
	_cfg, err := os.ReadFile("config.yaml")
	if err != nil {
		log.Fatal().Err(err).Msg("failed to read config file")
	}

	err = yaml.Unmarshal(_cfg, &cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to parse config file")
	}

	// 子命令分发
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "import":
			if len(os.Args) != 3 {
				fmt.Fprintln(os.Stderr, "usage: SOJ import <package-dir>")
				os.Exit(2)
			}
			if err := deploy.ImportPackage(&cfg, os.Args[2]); err != nil {
				log.Fatal().Err(err).Msg("import failed")
			}
			return
		case "rejudge":
			opts := deploy.RejudgeOptions{}
			if len(os.Args) == 3 {
				opts.ProblemId = os.Args[2]
			} else if len(os.Args) != 2 {
				fmt.Fprintln(os.Stderr, "usage: SOJ rejudge [problem_id]")
				os.Exit(2)
			}
			if err := deploy.Rejudge(&cfg, opts); err != nil {
				log.Fatal().Err(err).Msg("rejudge failed")
			}
			return
		}
	}

	// 解析SSH公钥
	var pubkey gossh.PublicKey
	if cfg.AllowedSSHPubkey != "" {
		pubkey, _, _, _, err = gossh.ParseAuthorizedKey([]byte(cfg.AllowedSSHPubkey))
		if err != nil {
			log.Fatal().Err(err).Msg("failed to parse allowed ssh pubkey")
		}
	} else {
		log.Warn().Msg("no allowed ssh pubkey specified, allowing all")
	}

	// 创建Apptainer服务
	sandboxService := file_transfer.NewApptainerService()

	// 解析主机密钥
	pk, err := gossh.ParsePrivateKey([]byte(cfg.HostKey))
	if err != nil {
		log.Fatal().Err(err).Msg("failed to parse host key")
	}

	// 初始化数据库服务
	dbService, err := types.NewDatabaseService(&cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize database service")
	}

	// 初始化问题管理器
	problemManager := judge.NewProblemManager()
	problems := problemManager.LoadProblemDir(cfg.ProblemsDir)

	// 执行全量用户扫描
	err = dbService.DoFullUserScan(problems)
	if err != nil {
		log.Error().Err(err).Msg("failed to perform full user scan")
	}

	// 初始化评测器
	evaluator := judge.NewEvaluator(&cfg, sandboxService, dbService)

	// 初始化HTTP服务器
	httpServer := ui.NewHTTPServer(dbService)
	httpServer.ServeHTTP(cfg.APIAddr)

	// 初始化SSH处理器
	sshHandler := ui.NewSSHHandler(dbService, &cfg, problems)

	// 设置SSH服务器
	s := &ssh.Server{
		Addr: cfg.ListenAddr,
		Handler: func(s ssh.Session) {
			// 处理特殊的submit命令
			cmds := s.Command()
			if len(cmds) >= 2 && (cmds[0] == "submit" || cmds[0] == "sub") {
				handleSubmit(s, &cfg, evaluator, problemManager, dbService, cmds)
			} else {
				sshHandler.HandleSession(s)
			}
		},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": func(sess ssh.Session) {
				file_transfer.SftpHandler(sess, &cfg, sandboxService)
			},
		},
		PublicKeyHandler: func(ctx ssh.Context, key ssh.PublicKey) bool {
			return pubkey == nil || ssh.KeysEqual(pubkey, key)
		},
	}
	s.AddHostKey(pk)

	log.Info().Str("addr", cfg.ListenAddr).Msg("listening")
	err = s.ListenAndServe()
	if err != nil {
		log.Fatal().Err(err).Msg("failed to listen")
	}
}

// handleSubmit 处理提交命令
func handleSubmit(s ssh.Session, cfg *types.Config, evaluator *judge.Evaluator, problemManager *judge.ProblemManager, dbService *types.DatabaseService, cmds []string) {
	uf := types.Userface{
		Buffer: bytes.NewBuffer(nil),
		Writer: s,
	}

	uf.Println(aurora.Yellow(time.Now().Format(time.DateTime + " MST")))

	if len(cmds) != 2 {
		uf.Println(aurora.Red("error:"), "invalid arguments")
		uf.Println("usage: submit <problem_id>")
		return
	}

	pid := cmds[1]

	pb, ok := problemManager.GetProblem(pid)
	if !ok {
		uf.Println(aurora.Red("error:"), "problem", aurora.Yellow(strconv.Quote(pid)), "not found")
		return
	}

	uf.Println(aurora.Green("Submitting"), aurora.Bold(pid))
	subtime := time.Now()

	id := strconv.Itoa(int(subtime.UnixNano()))
	ctx := types.SubmitCtx{
		ID:      id,
		Problem: pid,
		User:    s.User(),

		SubmitTime: subtime.UnixNano(),

		Status: "init",

		SubmitDir: path.Join(cfg.SubmitsDir, s.User(), pid),
		Workdir:   path.Join(cfg.SubmitWorkDir, id),

		RealWorkdir: path.Join(cfg.RealSubmitWorkDir, id),

		Userface: types.Userface{
			Buffer: bytes.NewBuffer(nil),
			Writer: uf,
		},
		Running: make(chan struct{}),
	}

	go evaluator.RunJudge(&ctx, &pb)

	<-ctx.Running

	uf.Println("Submit", "is", types.ColorizeStatus(ctx.Status))
	uf.Println("Message:\n	", aurora.Blue(ctx.Msg))

	writeResult(uf, ctx)

	// 更新用户数据
	err := dbService.UpdateUserSubmitResult(s.User(), &ctx, &pb)
	if err != nil {
		log.Error().Err(err).Str("user", s.User()).Msg("failed to update user submit result")
	}
}

// writeResult 写入结果
func writeResult(uf types.Userface, res types.SubmitCtx) {
	if res.Status != "completed" {
		uf.Println(aurora.Italic(aurora.Underline(aurora.Bold(aurora.Gray(15, "No judgement result")))))
		uf.Println()
		return
	}
	if res.JudgeResult.Success {
		uf.Printf("Score %.2f %s\n", aurora.Underline(aurora.Bold(types.ColorizeScore(res.JudgeResult))), aurora.Italic(aurora.Gray(15, "max.100 (Unweighted)")))
	} else {
		uf.Println(aurora.Red("Judgement is Failed"))
	}

	uf.Println("Judgement Message:")

	if len(res.JudgeResult.Msg) > 0 {
		uf.Println(aurora.Bold(aurora.Cyan("	" + fmt.Sprintf("%s", res.JudgeResult.Msg))))
	} else {
		uf.Println("	", aurora.Gray(15, "No message"))
	}
	uf.Println()
}
