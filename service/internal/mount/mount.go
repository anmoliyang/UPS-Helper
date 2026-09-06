// Package mount 封装 usbip attach / detach 的调用与 vhci 端口查询。
//
// 关键设计：usbip attach 在成功导入设备后不会退出——它作为长驻进程持续
// 转发 URB，进程退出即设备被内核摘除。因此 Attach 使用 cmd.Start() 启动后
// 立即返回（不等待退出），由后台 goroutine 监控进程生命周期；Detach 则先
// usbip detach 摘除内核设备，再终止 attach 进程兜底。
package mount

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"UPS-Helper/internal/config"
	"UPS-Helper/internal/logx"
)

// AttachDelay 是服务启动后延迟执行 attach 的时间。
// 挂载本身在 NUT 就绪后数百毫秒内即完成，过长的延迟只会增加用户提前去飞牛
// UPS 设置页开关、进而触发飞牛缓存空 UPS 的概率。故收紧到 500ms
// （仅留作启动期让 USB/IP 服务端彻底就绪的极小宽限）。
const AttachDelay = 500 * time.Millisecond

const cmdTimeout = 15 * time.Second

// attachConfirmTimeout 是 attach 启动后等待设备出现在 usbip port 的最大时长。
const attachConfirmTimeout = 5 * time.Second

// usbipBin 返回要使用的 usbip 客户端二进制路径；找不到随包二进制时返回错误。
//
// 只信任随包分发的静态二进制，或由 cmd/main 以绝对路径显式注入的路径。
// **绝不回退到 PATH 中的 "usbip"**：本进程以 root 运行，一旦 PATH 上出现同名
// 恶意程序（PATH 劫持），就会被 root 直接执行。宁可让自动挂载失败并明确报错，
// 也不执行来源不明的同名命令。
func usbipBin() (string, error) {
	// 次级来源：cmd/main 注入的环境变量（兼容经 cmd/main 启动的场景）。
	// 该变量由我们自己的启动脚本设置，属于可信来源。
	if env := strings.TrimSpace(os.Getenv("UPS_USBIP_BIN")); env != "" {
		if info, err := os.Stat(env); err == nil && !info.IsDir() {
			return env, nil
		}
	}
	// 首选：飞牛官方拓扑。cmd/ 固定在 /var/apps/<appname>/cmd/，而后端二进制
	// 实际位于 /volN/@appcenter/<appname>/target/UPS-Helper（UI 目录同理），两者
	// 并非同父兄弟——仅靠 os.Executable() 推导 ../cmd/usbip 会算出并不存在的
	// /volN/@appcenter/<appname>/cmd/usbip，导致自动挂载"找不到 usbip"而失败。
	// 故与 cmd/install_init 的探测顺序保持一致，优先按官方路径定位。
	if app := strings.TrimSpace(os.Getenv("TRIM_APPNAME")); app != "" {
		cand := filepath.Join("/var/apps", app, "cmd", "usbip")
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand, nil
		}
	}
	// 兜底：应用名固定，用于 TRIM_APPNAME 未注入的场景。
	if info, err := os.Stat("/var/apps/UPS-Helper/cmd/usbip"); err == nil && !info.IsDir() {
		return "/var/apps/UPS-Helper/cmd/usbip", nil
	}
	// 末选：从可执行文件所在目录逐级向上找 cmd/usbip（覆盖其它部署形态）。
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		dir := filepath.Dir(exe)
		for i := 0; i < 4; i++ {
			cand := filepath.Join(dir, "cmd", "usbip")
			if info, err := os.Stat(cand); err == nil && !info.IsDir() {
				return cand, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", errors.New("未找到随包分发的 usbip 客户端二进制（已尝试 /var/apps/<appname>/cmd/usbip 等路径），" +
		"已拒绝回退到 PATH 中的同名命令以避免以 root 执行来源不明的程序；请重新安装本应用")
}

// Manager 负责维护自动挂载状态。
type Manager struct {
	mu        sync.Mutex
	target    config.MountTarget
	vhciPort  int
	attached  bool
	attachCmd *exec.Cmd // 长驻的 usbip attach 进程（未启动时为 nil）
}

// NewManager 创建挂载管理器。
func NewManager(target config.MountTarget) *Manager {
	return &Manager{target: target, vhciPort: -1}
}

// Target 返回当前挂载目标。
func (m *Manager) Target() config.MountTarget {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.target
}

// Attached 返回是否已挂载成功。
func (m *Manager) Attached() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attached
}

// AttachAfterDelay 延迟 AttachDelay 后执行 attach，ctx 取消则放弃。
func (m *Manager) AttachAfterDelay(ctx context.Context) {
	logx.Infof("自动挂载将在 %s 后执行: %s", AttachDelay, m.Target().String())
	select {
	case <-ctx.Done():
		return
	case <-time.After(AttachDelay):
	}
	if err := m.Attach(ctx); err != nil {
		logx.Errorf("自动挂载失败: %v", err)
	}
}

// Attach 启动长驻的 usbip attach 进程，并轮询 usbip port 确认设备挂载成功。
// 进程启动后不等待退出（attach 需要常驻转发 URB），由后台 goroutine
// 监控生命周期；ctx 取消或确认超时时会终止该进程。
func (m *Manager) Attach(ctx context.Context) error {
	t := m.Target()

	// 目标设备已在 vhci 端口：仅当它是本 Manager 刚 attach（attachCmd 存活）时
	// 才复用；否则（如服务重启后残留的旧设备）必须先 detach 再重新 attach，
	// 强制内核重新枚举，主机才能读到更新后的 iProduct（UPS 型号）。
	m.mu.Lock()
	owned := m.attachCmd != nil
	m.mu.Unlock()

	if owned {
		if port := findVhciPort(ctx, t); port >= 0 {
			m.mu.Lock()
			m.attached = true
			m.vhciPort = port
			m.mu.Unlock()
			logx.Infof("目标设备已在 vhci 端口 %d，复用现有挂载", port)
			return nil
		}
	} else if port := findVhciPort(ctx, t); port >= 0 {
		logx.Warnf("检测到残留的虚拟 USB 设备（vhci 端口 %d），先摘除再重新挂载以应用最新配置", port)
		if bin, berr := usbipBin(); berr != nil {
			logx.Warnf("跳过摘除残留设备: %v", berr)
		} else if out, err := run(ctx, bin, "detach", "--port", strconv.Itoa(port)); err != nil {
			logx.Warnf("摘除残留设备失败: %v (输出: %s)", err, compact(out))
		}
		// 等内核清理完成
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	args := []string{}
	// usbip 的 --tcp-port 是全局选项，需放在子命令之前；默认端口时省略以贴合标准用法。
	if t.Port != config.DefaultServerPort {
		args = append(args, "--tcp-port", strconv.Itoa(t.Port))
	}
	args = append(args, "attach", "--remote", t.Host, "--busid", t.BusID)

	bin, err := usbipBin()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("usbip attach 启动失败(二进制=%s): %w", bin, err)
	}
	logx.Infof("usbip attach 已启动 (pid=%d): %s %s", cmd.Process.Pid, bin, strings.Join(args, " "))

	// 保存进程句柄，供 Detach 终止使用
	m.mu.Lock()
	m.attachCmd = cmd
	m.mu.Unlock()

	// 后台监控：进程退出（正常 detach 或意外崩溃）时清理挂载状态
	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		if m.attachCmd == cmd {
			m.attachCmd = nil
			m.attached = false
			m.vhciPort = -1
		}
		m.mu.Unlock()
		if err != nil {
			logx.Warnf("usbip attach 进程退出: %v (输出: %s)", err, compact(buf.String()))
		} else {
			logx.Infof("usbip attach 进程已退出")
		}
	}()

	// 轮询确认挂载成功（最多 attachConfirmTimeout）。
	// 成功判据：usbip attach 进程持续存活即代表设备已挂入内核——这是 usbip 的固有
	// 语义（进程不退出代表连接/转发正常，进程退出才代表失败）。故不再把 `usbip port`
	// 的输出解析作为挂载成功的硬性前提，避免不同 usbip 客户端实现输出格式差异导致
	// 误判超时、killAttach 把刚挂上的设备反手摘掉（表现为「自动挂载后飞牛仍看不到设备」）。
	deadline := time.Now().Add(attachConfirmTimeout)
	for time.Now().Before(deadline) {
		// 进程已提前退出且未确认挂载：attach 失败，立即上报
		m.mu.Lock()
		alive := m.attachCmd != nil
		m.mu.Unlock()
		if !alive {
			return fmt.Errorf("usbip attach 进程提前退出 (输出: %s)", compact(buf.String()))
		}

		// 优先用 usbip port 拿到端口号（仅用于展示/Detach，解析失败不影响挂载判定）
		if port := findVhciPort(ctx, t); port >= 0 {
			m.mu.Lock()
			m.attached = true
			m.vhciPort = port
			m.mu.Unlock()
			logx.Infof("虚拟 USB 设备已挂载到 vhci 端口 %d", port)
			return nil
		}

		select {
		case <-ctx.Done():
			m.killAttach()
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	// 超时但进程仍存活：以「进程存活」为最终判据，认定挂载成功，
	// 仅尽力探测端口号（失败则留 -1，Detach 时兜底用 0）。
	m.mu.Lock()
	alive := m.attachCmd != nil
	m.mu.Unlock()
	if alive {
		port := findVhciPort(ctx, t)
		m.mu.Lock()
		m.attached = true
		m.vhciPort = port
		m.mu.Unlock()
		logx.Infof("虚拟 USB 设备已挂载（依赖 attach 进程存活判据，端口探测=%d）", port)
		return nil
	}
	m.killAttach()
	return fmt.Errorf("usbip attach 超时未挂载成功 (输出: %s)", compact(buf.String()))
}

// Detach 摘除已挂载的设备并终止 attach 进程。未挂载时仅兜底清理进程。
func (m *Manager) Detach(ctx context.Context) error {
	m.mu.Lock()
	attached := m.attached
	port := m.vhciPort
	t := m.target
	m.mu.Unlock()

	// 1) 摘除内核设备（若有）
	if attached {
		if port < 0 {
			// 兜底：重新查询一次，仍失败则用 0
			if p := findVhciPort(ctx, t); p >= 0 {
				port = p
			} else {
				port = 0
			}
		}
		if bin, berr := usbipBin(); berr != nil {
			logx.Warnf("跳过摘除设备: %v", berr)
		} else if out, err := run(ctx, bin, "detach", "--port", strconv.Itoa(port)); err != nil {
			logx.Warnf("usbip detach --port %d 失败: %v (输出: %s)", port, err, compact(out))
		} else {
			logx.Infof("usbip detach 成功 (vhci 端口 %d)", port)
		}
	}

	// 2) 终止长驻的 attach 进程（正常 detach 后进程应自行退出，这里兜底）
	m.killAttach()

	m.mu.Lock()
	m.attached = false
	m.vhciPort = -1
	m.mu.Unlock()
	return nil
}

// killAttach 终止长驻的 usbip attach 进程并清理状态。
// 只调用 Kill()，不调用 Wait()——Wait 由监控 goroutine 独占（Wait 只能调用一次）。
func (m *Manager) killAttach() {
	m.mu.Lock()
	cmd := m.attachCmd
	m.attachCmd = nil
	m.attached = false
	m.vhciPort = -1
	m.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// portLineRe 匹配 `Port 00: <Port in Use> at Full Speed(12Mbps)`
var portLineRe = regexp.MustCompile(`^Port\s+(\d+):`)

// findVhciPort 解析 `usbip port` 输出，找出与目标 host:port/busid 对应的 vhci 端口号。
func findVhciPort(ctx context.Context, t config.MountTarget) int {
	bin, berr := usbipBin()
	if berr != nil {
		logx.Debugf("无法查询 vhci 端口: %v", berr)
		return -1
	}
	out, err := run(ctx, bin, "port")
	if err != nil {
		logx.Debugf("执行 usbip port 失败: %v (输出: %s)", err, out)
		return -1
	}
	// 期望在某个 Port NN 段落中出现 usbip://<host>:<port>/<busid>
	needle := fmt.Sprintf("usbip://%s:%d/%s", t.Host, t.Port, t.BusID)
	current := -1
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if mt := portLineRe.FindStringSubmatch(trimmed); mt != nil {
			if n, err := strconv.Atoi(mt[1]); err == nil {
				current = n
			}
			continue
		}
		if current >= 0 && strings.Contains(trimmed, needle) {
			return current
		}
	}
	// 退化匹配：只按 busid 找
	current = -1
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if mt := portLineRe.FindStringSubmatch(trimmed); mt != nil {
			if n, err := strconv.Atoi(mt[1]); err == nil {
				current = n
			}
			continue
		}
		if current >= 0 && strings.Contains(trimmed, "/"+t.BusID) && strings.Contains(trimmed, "usbip://") {
			return current
		}
	}
	return -1
}

// run 执行外部命令并合并返回 stdout/stderr。
func run(ctx context.Context, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	logx.Debugf("执行命令: %s %s", name, strings.Join(args, " "))
	err := cmd.Run()
	return buf.String(), err
}

func compact(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "<无输出>"
	}
	return strings.Join(strings.Fields(s), " ")
}
