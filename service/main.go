// 命令 UPS-Helper 是“UPS 助手”的核心服务进程：
// 启动 USB/IP 服务端（模拟 HID UPS 设备）、NUT 轮询、Web 管理服务与自动挂载，
// 并在收到 SIGINT/SIGTERM 时优雅退出（先 detach 再关闭 USB/IP 服务）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"UPS-Helper/internal/app"
	"UPS-Helper/internal/cgi"
	"UPS-Helper/internal/config"
	"UPS-Helper/internal/logx"
)

func main() {
	// 飞牛第三方应用入口约定：框架以 `cmd/main start|stop|status` 管理应用。
	// start 需后台化（setsid 脱离终端）并写入 pidfile，父进程立即退出，
	// 避免调用方（upgrade_callback / 安装框架）因进程前台阻塞而卡死
	// （表现为应用中心安装/升级卡在 85%）。
	//
	// stop / status 必须在此处处理：框架靠它们停止应用与判定运行状态。若漏掉，
	// 这两个子命令会落到下面的服务启动逻辑被当作命令行参数解析，从而又拉起一个
	// 服务进程（端口已被占用随即崩溃退出）——表现为应用一直卡在「启用中」、
	// 关不掉、且框架认为未运行而拒绝反代，Web 入口打不开。
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "start":
			if err := daemonize(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "后台启动失败: %v\n", err)
				os.Exit(1)
			}
			os.Exit(0)
		case "stop":
			if err := stopDaemon(); err != nil {
				fmt.Fprintf(os.Stderr, "停止失败: %v\n", err)
				os.Exit(1)
			}
			os.Exit(0)
		case "status":
			// 飞牛约定：运行中退出码 0，未运行退出码 3。
			if daemonRunning() {
				os.Exit(0)
			}
			os.Exit(3)
		}
	}

	cfgPath := flag.String("c", config.ResolvePath(), "配置文件路径 (JSON)")
	debug := flag.Bool("d", false, "启用调试日志")
	help := flag.Bool("h", false, "显示帮助信息并退出")
	cgiMode := flag.Bool("cgi", false, "以 CGI 模式运行（由 app/ui/index.cgi 调用，转发请求到后端）")
	flag.Parse()

	// CGI 模式必须最先判断并直接返回：它由飞牛 Web 服务器逐请求拉起，
	// 转发完即退出。此处若先写任何日志都会污染 CGI 响应体。
	if *cgiMode {
		cgi.Serve()
		return
	}

	if *help {
		printUsage()
		return
	}

	logx.SetDebug(*debug)

	// 把日志同时落盘到应用数据目录：日志默认只写 stdout，被飞牛框架接管后用户
	// 根本取不到（此前反复 cat /var/apps/.../app.log 都是 No such file），
	// 排障只能靠猜。落盘后用户可直接 cat 取证。必须在首条日志之前启用。
	if lp := logFilePath(); lp != "" {
		if err := logx.EnableFile(lp); err != nil {
			logx.Warnf("日志文件 %s 打开失败（仅写 stdout）: %v", lp, err)
		}
	}

	logx.Infof("UPS助手 (UPS-Helper) v%s 启动中（配置文件 %s）", app.Version, *cfgPath)

	// 删除位于硬编码路径 /etc/UPS助手/config.json 的孤立配置（若存在且非当前生效路径）：
	// 该路径不在飞牛托管目录内，卸载时无法被清理，会残留用户填写的 remote_ups。
	// 配置现已统一到 TRIM_PKGETC，此路径已是无人读取的孤立文件，主动删除以清除私人数据。
	removeLegacyConfig(*cfgPath)

	cfg, err := loadOrInitConfig(*cfgPath)
	if err != nil {
		logx.Fatalf("初始化配置失败: %v", err)
	}

	// 持续把当前配置镜像到 var 备份（升级不清），确保升级时 etc 被清空后仍能还原，
	// 不再单点依赖 upgrade_init 钩子。详见 config.MirrorBackup。
	if err := config.MirrorBackup(cfg, *cfgPath); err != nil {
		logx.Warnf("镜像配置备份失败（升级时将无法还原配置）: %v", err)
	}

	// 配置校验不通过时仍启动服务，让用户在 Web 界面中修正；NUT 轮询会自动
	// 进入“断连”状态而不会导致进程崩溃。
	if err := cfg.Validate(); err != nil {
		logx.Warnf("配置校验未通过: %v（服务继续启动，请在 Web 界面修正）", err)
	}

	// 捕获 SIGINT / SIGTERM 以实现优雅退出：cancel 触发 App.shutdown，
	// 其内部按“先 detach 再关 USB/IP”的顺序停止子系统。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 记录自身 pid：飞牛可能不经 `main start` 而直接拉起本进程，
	// 此时 daemonize 分支不会执行、也就没人写 pidfile，导致 `main status` 读不到
	// pid 而误判为未运行（退出码 3），框架便认为应用没起来、不给反代入。
	// 这里补写一次，保证无论以何种方式启动，status 都能正确判定。
	if err := writePIDFile(); err != nil {
		logx.Warnf("写入 pidfile 失败: %v", err)
	}

	application := app.New(cfg, *cfgPath, app.DefaultUIDir())
	if err := application.Run(ctx); err != nil {
		logx.Fatalf("服务运行失败: %v", err)
	}

	logx.Infof("UPS助手已退出")
}

// defaultPIDFile 返回 pid 文件默认路径，与生命周期脚本（upgrade/uninstall_callback）
// 的约定保持一致：TRIM_PKGVAR 环境变量下的 UPS-Helper.pid（未注入时回退到 /var/lib）。
func defaultPIDFile() string {
	base := os.Getenv("TRIM_PKGVAR")
	if base == "" {
		base = "/var/lib/UPS-Helper"
	}
	return filepath.Join(base, "UPS-Helper.pid")
}

// writePIDFile 把当前进程号写入 pidfile，供 `main status` / `main stop` 使用。
func writePIDFile() error {
	path := defaultPIDFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建 pidfile 目录失败: %w", err)
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// daemonize 以自身重新 exec 到新的 session（setsid），使进程脱离控制终端成为守护进程，
// 并在父进程中把子进程 pid 写入 pidfile 后退出。父进程退出可让调用方（如
// upgrade_callback 中直接 `$SELF_DIR/main start` 的同步调用）立即返回，
// 不会因服务前台运行而卡死（表现为安装/升级卡在 85%）。
// rest 为传给子进程的参数（如 -c 配置文件路径），不含已被消费的 "start" 子命令。
func daemonize(rest []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取自身路径失败: %w", err)
	}
	cmd := exec.Command(exe, rest...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动守护进程失败: %w", err)
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(defaultPIDFile(), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("写入 pidfile 失败: %w", err)
	}
	return nil
}

// stopWaitTimeout 需大于服务优雅退出所需时间：收到 SIGTERM 后要先 usbip detach
// 再关闭 USB/IP 服务端，detach 本身有最长 20 秒的超时上下文。
const stopWaitTimeout = 30 * time.Second

// readPIDFile 读取 pidfile 中的进程号；文件缺失或内容非法时返回 0。
func readPIDFile() int {
	data, err := os.ReadFile(defaultPIDFile())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// daemonRunning 判断守护进程是否仍在运行（信号 0 只做存在性检查，不发信号）。
func daemonRunning() bool {
	pid := readPIDFile()
	if pid == 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// stopDaemon 停止守护进程：先 SIGTERM 触发优雅退出（先 usbip detach 再关 USB/IP），
// 超时后 SIGKILL 兜底并清理 pidfile。进程本就不存在时视为成功。
func stopDaemon() error {
	pid := readPIDFile()
	if pid == 0 {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		// 进程已不存在，清理可能残留的 pidfile。
		_ = os.Remove(defaultPIDFile())
		return nil
	}
	deadline := time.Now().Add(stopWaitTimeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			_ = os.Remove(defaultPIDFile())
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	// 优雅退出超时：强制终止。此时内核侧可能仍挂着虚拟设备，
	// uninstall_init 会再做一次 detach 兜底。
	_ = syscall.Kill(pid, syscall.SIGKILL)
	_ = os.Remove(defaultPIDFile())
	return nil
}

// loadOrInitConfig 读取配置文件；若文件不存在则写入默认配置后返回，
// 以便首次安装（尚未经向导填写）也能启动服务并由 Web 界面接管配置。
// 同时确保 API token 存在（配置中缺失时自动生成随机令牌）。
func loadOrInitConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 配置缺失：可能是升级后 etc 被重置。若 var 备份含有效配置则先还原，
			// 备份仅在升级路径存在（真正卸载会删除），全新安装无备份则走默认。
			if cfg2, restored, rerr := restoreFromVarBackup(path); rerr != nil {
				return cfg, rerr
			} else if restored {
				logx.Infof("配置缺失，已从 var 备份还原（remote_ups=%s）", cfg2.RemoteUPS)
				return cfg2, nil
			}
			logx.Warnf("配置文件 %s 不存在，将创建默认配置", path)
			cfg = config.Default()
			if _, err := cfg.EnsureAPIToken(); err != nil {
				return cfg, fmt.Errorf("初始化 API token 失败: %w", err)
			}
			if err := cfg.Save(path); err != nil {
				return cfg, fmt.Errorf("写入默认配置失败: %w", err)
			}
			logx.Infof("已写入默认配置文件 %s", path)
			return cfg, nil
		}
		return cfg, err
	}
	// 文件存在但 remote_ups 为空：可能是升级后 etc 被重置为空模板。若 var 备份
	// 含有效配置则还原，避免升级丢失「UPS 名称@IP」。
	if cfg.RemoteUPS == "" {
		if cfg2, restored, rerr := restoreFromVarBackup(path); rerr != nil {
			return cfg, rerr
		} else if restored {
			logx.Infof("配置无 remote_ups，已从 var 备份还原（remote_ups=%s）", cfg2.RemoteUPS)
			cfg = cfg2
		}
	}
	// http_bind 若指向非回环地址则收敛为回环：入口经飞牛反代，后端只需对回环提供服务，
	// 对外暴露写操作接口会带来越权风险。
	if cfg.MigrateHTTPBindToLoopback() {
		if err := cfg.Save(path); err != nil {
			return cfg, fmt.Errorf("收敛 http_bind 到回环失败: %w", err)
		}
		logx.Infof("已将 http_bind 收敛为回环 %s（入口由飞牛反代，无需对外暴露端口）", config.DefaultHTTPBind)
	}
	// 配置中若尚缺 api_token，补生成随机令牌。
	generated, err := cfg.EnsureAPIToken()
	if err != nil {
		return cfg, fmt.Errorf("生成 API token 失败: %w", err)
	}
	if generated {
		if err := cfg.Save(path); err != nil {
			return cfg, fmt.Errorf("写入 API token 失败: %w", err)
		}
		logx.Infof("已为配置文件生成 API token: %s", path)
	}
	logx.Infof("已加载配置文件 %s", path)
	return cfg, nil
}

// restoreFromVarBackup 当 etc 配置缺失或 remote_ups 为空、而 var 备份仍含有效
// 连接配置时，用备份还原 etc 配置（原子写回），返回还原后的配置与是否还原。
// 备份由运行期镜像与升级流程维护，仅在升级路径存在——真正卸载时 uninstall_callback
// 会删除备份，故卸载后全新安装不会触发还原，不泄漏曾填写的连接信息。
func restoreFromVarBackup(cfgPath string) (config.Config, bool, error) {
	dir := strings.TrimSpace(os.Getenv("TRIM_PKGVAR"))
	if dir == "" {
		dir = "/var/lib/UPS-Helper"
	}
	bakCfg, err := config.Load(filepath.Join(dir, "UPS-Helper-config.bak"))
	if err != nil {
		return config.Config{}, false, nil // 无可用备份（全新安装），非错误
	}
	if strings.TrimSpace(bakCfg.RemoteUPS) == "" {
		return config.Config{}, false, nil // 备份无连接配置，无还原价值
	}
	if err := bakCfg.Save(cfgPath); err != nil {
		return config.Config{}, false, fmt.Errorf("写回还原配置失败: %w", err)
	}
	return bakCfg, true, nil
}

// logFilePath 返回运行日志落盘路径：应用数据目录下的 log/app.log。
// 目录取自飞牛注入的 TRIM_PKGVAR（与生命周期脚本同一约定），未注入时回退到
// /var/apps/UPS-Helper/var —— 与用户排查时期望的 /var/apps/UPS-Helper/var/log/app.log
// 一致，便于直接 cat。
func logFilePath() string {
	base := strings.TrimSpace(os.Getenv("TRIM_PKGVAR"))
	if base == "" {
		base = "/var/apps/UPS-Helper/var"
	}
	return filepath.Join(base, "log", "app.log")
}

// removeLegacyConfig 删除位于硬编码路径 /etc/UPS助手/config.json 的孤立配置（若存在）。
//
// 应用配置统一存放于飞牛托管目录 TRIM_PKGETC，卸载时由框架清理；而该硬编码路径处于
// 托管目录之外，即便曾被写入过，卸载也无法触及，会残留用户填写的 remote_ups。
// 当当前生效路径并非该硬编码路径时，说明它已是孤立文件，可直接删除以清除私人数据残留。
// 仅当生效路径确实不是该硬编码路径时才删除（本地非飞牛环境不动）。
func removeLegacyConfig(activePath string) {
	if activePath == config.DefaultPath {
		return // 生效路径就是遗留路径本身（未注入 TRIM_PKGETC），不能删
	}
	if _, err := os.Stat(config.DefaultPath); err != nil {
		return // 遗留文件不存在，无需处理
	}
	if err := os.Remove(config.DefaultPath); err != nil {
		logx.Warnf("删除孤立配置 %s 失败: %v", config.DefaultPath, err)
		return
	}
	logx.Infof("已删除孤立配置 %s（配置统一到飞牛托管目录，卸载才能真正清空）", config.DefaultPath)
}

func printUsage() {
	fmt.Fprintf(os.Stdout, `UPS助手 (UPS-Helper) — 飞牛 fnOS 原生 UPS 远程代理

用法:
  UPS-Helper [-c 配置文件] [-d] [-h]

参数:
  -c  配置文件路径 (默认 %s)
  -d  启用调试日志
  -h  显示本帮助并退出

其余参数均从配置文件读取:
  remote_ups        远程 NUT UPS 标识, 格式 ups_name@host[:nut_port]
  server_port       USB/IP 服务监听端口 (默认 %d)
  server_bind       USB/IP 监听地址 (默认 %s, 仅本机; 跨机 USB/IP 才改 0.0.0.0)
  auto_mount        启动后是否自动 usbip attach (默认 开)
  auto_mount_target 自动挂载目标, 格式 host[:usbip_port]@bus-id (默认 %s)
  http_port         Web 管理界面端口 (默认 %d)
  http_bind         Web 监听地址 (默认 %s, 必须保持回环)
                    — 入口由飞牛 Nginx 反代（app/ui/config 不声明 port，
                      页面经 /cgi/ThirdParty/UPS-Helper/index.cgi/ 加载，处于飞牛
                      登录会话保护之内），后端只需对回环提供服务。
                      改为 0.0.0.0 会绕过飞牛登录、把写操作接口暴露给局域网，
                      故该字段不允许经 Web API 修改（只能改配置文件）。
`,
		config.ResolvePath(),
		config.DefaultServerPort,
		config.DefaultServerBind,
		config.DefaultAutoMountTarget,
		config.DefaultHTTPPort,
		config.DefaultHTTPBind,
	)
}
