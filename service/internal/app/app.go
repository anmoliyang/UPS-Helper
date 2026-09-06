// Package app 编排各子系统：NUT 轮询、USB/IP 服务端、自动挂载与 Web 管理服务，
// 并实现配置热重载（修改配置无需重启进程）。
//
// 设计要点：
//   - Bridge 持有共享状态（配置、UPS 状态缓存、虚拟设备）与各子系统句柄。
//   - 每个长期运行子系统由父 context 驱动；context 取消即停止（context 驱动生命周期）。
//   - 监听类子系统（USB/IP、Web）通过 listenSubsystem 统一启动；bind 失败会被
//     ListenAndServe 同步返回，从而立即上报而非靠额外的 WaitError 通道。
//   - 配置热重载 ApplyConfig 仅局部重启受影响的子系统，并对端口类变更做回滚，
//     尽可能保持服务可用性。
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"UPS-Helper/internal/config"
	"UPS-Helper/internal/httpapi"
	"UPS-Helper/internal/logx"
	"UPS-Helper/internal/mount"
	"UPS-Helper/internal/nut"
	"UPS-Helper/internal/ups"
	"UPS-Helper/internal/usbdev"
	"UPS-Helper/internal/usbip"
)

// listenProbeTimeout 是启动监听类子系统后等待 bind 失败暴露的窗口。
const listenProbeTimeout = 300 * time.Millisecond

// stopTimeout 是等待子系统 goroutine 退出的上限，超时仅记日志不阻塞。
const stopTimeout = 5 * time.Second

// subsystem 是一个由 context 驱动的长期运行任务。
type subsystem struct {
	cancel context.CancelFunc
	done   chan struct{}
	errCh  chan error
}

// spawn 启动一个子系统 goroutine，父 context 取消即停止。
// fn 内部应通过传入的 ctx 感知取消；返回非取消类错误时记日志并写入 errCh。
func spawn(ctx context.Context, fn func(context.Context) error) *subsystem {
	sctx, cancel := context.WithCancel(ctx)
	s := &subsystem{cancel: cancel, done: make(chan struct{}), errCh: make(chan error, 1)}
	go func() {
		defer close(s.done)
		if err := fn(sctx); err != nil && sctx.Err() == nil {
			logx.Errorf("子系统异常退出: %v", err)
			select {
			case s.errCh <- err:
			default:
			}
		}
	}()
	return s
}

// stop 取消并等待子系统退出（带超时）。
func (s *subsystem) stop() {
	if s == nil {
		return
	}
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(stopTimeout):
		logx.Warnf("子系统停止超时")
	}
}

// awaitStart 给监听类子系统一点时间以暴露 bind 失败；超时视为启动成功。
// ListenAndServe 在 net.Listen 失败时同步返回错误，故此窗口只需极短。
func (s *subsystem) awaitStart(timeout time.Duration) error {
	select {
	case err := <-s.errCh:
		s.stop()
		return err
	case <-time.After(timeout):
		return nil
	}
}

// Bridge 是服务主体，负责编排与对外暴露配置/状态接口。
type Bridge struct {
	cfgPath string
	uiDir   string

	state *ups.State
	dev   *usbdev.Device

	startedAt time.Time

	mu      sync.Mutex
	cfg     config.Config
	rootCtx context.Context

	nutRun   *subsystem
	usbRun   *subsystem
	httpRun  *subsystem
	usbipSrv *usbip.Server
	mountMgr *mount.Manager
	mountCtl context.CancelFunc
}

// New 创建 Bridge 实例。cfgPath 用于配置落盘，uiDir 为 Web 静态资源目录。
func New(cfg config.Config, cfgPath, uiDir string) *Bridge {
	state := ups.NewState()
	dev := usbdev.New(state)
	return &Bridge{
		cfgPath:   cfgPath,
		uiDir:     uiDir,
		cfg:       cfg,
		state:     state,
		dev:       dev,
		startedAt: time.Now(),
	}
}

// Run 启动全部子系统并阻塞，直到 ctx 取消后优雅退出。
func (b *Bridge) Run(ctx context.Context) error {
	b.mu.Lock()
	b.rootCtx = ctx
	cfg := b.cfg
	b.mu.Unlock()

	logx.Infof("配置: remote_ups=%s server_port=%d http_port=%d auto_mount=%v auto_mount_target=%s",
		cfg.RemoteUPS, cfg.ServerPort, cfg.HTTPPort, cfg.AutoMount, cfg.AutoMountTarget)

	if err := b.startUSBIP(ctx, cfg); err != nil {
		return err
	}
	b.startNUT(ctx, cfg)
	if err := b.startHTTP(ctx, cfg); err != nil {
		b.stopUSBIP()
		return err
	}
	b.startAutoMount(ctx, cfg)

	<-ctx.Done()
	b.shutdown()
	return nil
}

// shutdown 按“先 detach 再关 USB/IP”的顺序优雅退出。
func (b *Bridge) shutdown() {
	logx.Infof("开始优雅退出...")

	b.stopAutoMount() // 1. 先摘挂载，让内核先摘除虚拟设备
	b.stopUSBIP()     // 2. 关闭 USB/IP 服务端

	// 3. 关闭 NUT 轮询与 Web 服务
	b.mu.Lock()
	nutRun, httpRun := b.nutRun, b.httpRun
	b.nutRun, b.httpRun = nil, nil
	b.mu.Unlock()
	nutRun.stop()
	httpRun.stop()

	logx.Infof("已全部停止")
}

// ---------------------------------------------------------------------------
// USB/IP 子系统
// ---------------------------------------------------------------------------

func (b *Bridge) startUSBIP(parent context.Context, cfg config.Config) error {
	srv := usbip.NewServer(b.dev)
	addr := fmt.Sprintf("%s:%d", cfg.ServerBind, cfg.ServerPort)

	b.mu.Lock()
	b.usbipSrv = srv
	b.usbRun = spawn(parent, func(c context.Context) error {
		return srv.ListenAndServe(c, addr)
	})
	b.mu.Unlock()

	if err := b.usbRun.awaitStart(listenProbeTimeout); err != nil {
		return fmt.Errorf("USB/IP 监听 %s 失败: %w", addr, err)
	}
	return nil
}

func (b *Bridge) stopUSBIP() {
	b.mu.Lock()
	t, srv := b.usbRun, b.usbipSrv
	b.usbRun, b.usbipSrv = nil, nil
	b.mu.Unlock()

	if srv != nil {
		srv.Close()
	}
	t.stop()
}

// ---------------------------------------------------------------------------
// NUT 子系统
// ---------------------------------------------------------------------------

func (b *Bridge) startNUT(parent context.Context, cfg config.Config) {
	target, err := config.ParseRemoteUPS(cfg.RemoteUPS)
	if err != nil {
		logx.Errorf("remote_ups 配置无效，NUT 轮询未启动: %v", err)
		b.state.SetDisconnected(err.Error())
		return
	}
	poller := nut.NewPoller(target, b.state, b.notifyReport, cfg.NUTUser, cfg.NUTPass)
	r := spawn(parent, func(c context.Context) error {
		poller.Run(c)
		return nil
	})

	b.mu.Lock()
	b.nutRun = r
	b.mu.Unlock()
}

func (b *Bridge) stopNUT() {
	b.mu.Lock()
	t := b.nutRun
	b.nutRun = nil
	b.mu.Unlock()
	t.stop()
}

// ---------------------------------------------------------------------------
// Web 子系统
// ---------------------------------------------------------------------------

func (b *Bridge) startHTTP(parent context.Context, cfg config.Config) error {
	srv := httpapi.New(b.uiDir, b, b)
	addr := fmt.Sprintf("%s:%d", cfg.HTTPBind, cfg.HTTPPort)
	// 入口走飞牛反代，后端应只对回环提供服务。绑到非回环会绕过飞牛
	// 登录会话，把写操作接口直接暴露给任何能访问该端口的人——必须显式告警。
	if cfg.HTTPBind != "127.0.0.1" && cfg.HTTPBind != "::1" && cfg.HTTPBind != "localhost" {
		logx.Warnf("警告：Web 服务绑定在 %q 而非回环地址，将绕过飞牛登录会话直接对外暴露写操作接口，请确认这是你期望的配置", cfg.HTTPBind)
	}

	b.mu.Lock()
	b.httpRun = spawn(parent, func(c context.Context) error {
		return srv.ListenAndServe(c, addr)
	})
	b.mu.Unlock()

	if err := b.httpRun.awaitStart(listenProbeTimeout); err != nil {
		return fmt.Errorf("web 服务监听 %s 失败: %w", addr, err)
	}
	return nil
}

func (b *Bridge) stopHTTP() {
	b.mu.Lock()
	t := b.httpRun
	b.httpRun = nil
	b.mu.Unlock()
	t.stop()
}

// ---------------------------------------------------------------------------
// 自动挂载子系统
// ---------------------------------------------------------------------------

// reenumCycles 是型号更新后反复摘挂重枚举的次数。飞牛按 VID:PID 缓存 UPS 显示名，
// 单次摘挂不一定能命中其重枚举时机，故做多次循环以提高刷新成功率。
const reenumCycles = 5

// reenumPause 是每次摘挂重枚举之间的间隔，留出时间让飞牛完成重新枚举（设备短暂消失
// 再出现，飞牛在其 UPS 轮询周期内重新读取我们下发的真实型号）。
const reenumPause = 3 * time.Second

func (b *Bridge) startAutoMount(parent context.Context, cfg config.Config) {
	if !cfg.AutoMount {
		logx.Infof("自动挂载已关闭，请手动执行: usbip attach --remote 127.0.0.1 --busid %s", usbdev.BusID)
		return
	}
	target, err := config.ParseMountTarget(cfg.AutoMountTarget, cfg.ServerPort)
	if err == nil {
		// 纵深防御：配置整体校验失败时服务仍会启动（保证 Web 界面可用来修正），
		// 故在真正以 root 执行 usbip attach 之前，再独立校验一次挂载目标，
		// 确保任何路径都不会让 root 去挂载公网主机上的 USB 设备。
		err = target.Validate()
	}
	if err != nil {
		logx.Errorf("auto_mount_target 无效，自动挂载未启动: %v", err)
		return
	}
	ctx, cancel := context.WithCancel(parent)

	b.mu.Lock()
	b.mountCtl = cancel
	b.mu.Unlock()

	// 绝不在 NUT 拿到真实型号之前挂载「占位身份」设备：占位设备（iProduct="UPS Remote"）
	// 会让飞牛首次扫描拿不到有效型号、缓存「无 UPS / 请接入 USB UPS 设备」，且此后仅靠
	// USB/IP 摘挂无法刷新该缓存。故改为「一直等到 NUT 上报真实型号再挂」：remote_ups
	// 未配置时 NUT 起不来、永远等不到型号，也就不挂（本就没有可桥接的 UPS）；用户一旦在
	// Web 界面填好 remote_ups，NUT 连上、型号就绪，waitAndAttach 立刻以真实身份挂载。
	// 飞牛被用户打开/开关 UPS 设置页触发首次扫描时，看到的就是身份就绪的 UPS，一次性识别。
	// 注：飞牛的 UPS 探测只在用户打开/开关 UPS 设置页时触发，vhci 热插拔事件不会被自动重扫，
	// 故应用侧唯一可控的杠杆是「确保首次扫描时设备已身份就绪」。
	go b.waitAndAttach(ctx, cfg, target)
}

// waitAndAttach 一直等到 NUT 上报真实型号后，才把虚拟设备挂上，确保飞牛首次扫描
// 就看到一个身份就绪的 UPS，避免「请接入 USB UPS 设备」式的识别失败。
//
// 为什么没有「超时兜底挂占位设备」：占位设备（iProduct="UPS Remote"）会让飞牛首次扫描
// 绑不出有效型号、缓存「无 UPS」，且此后仅靠摘挂无法刷新。故宁可永远等——
// remote_ups 未配置时 NUT 起不来、型号永远为空，本就不该挂载（没有可桥接的 UPS）；
// 一旦用户在 Web 界面填好 remote_ups，NUT 连上、型号非空，这里立即以真实身份挂载。
// 仅在 ctx 取消（停止/卸载）时退出。
func (b *Bridge) waitAndAttach(ctx context.Context, cfg config.Config, target config.MountTarget) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snap := b.state.Get()
			if snap.Model == "" {
				continue
			}
			// 双保险：挂载前置把真实型号/厂商/序列号同步到模拟设备，
			// 避免与 notifyReport 的并发竞争导致以旧（占位）身份挂载。
			b.applyDeviceIdentity(snap)
			logx.Infof("NUT 已就绪（型号 %q），以真实身份挂载虚拟设备", snap.Model)
			mgr := mount.NewManager(target)
			b.mu.Lock()
			b.mountMgr = mgr
			b.mu.Unlock()
			mgr.AttachAfterDelay(ctx)
			return
		}
	}
}

// stopAutoMount 取消延迟挂载并执行 detach。
func (b *Bridge) stopAutoMount() {
	b.mu.Lock()
	mgr, cancel := b.mountMgr, b.mountCtl
	b.mountMgr, b.mountCtl = nil, nil
	b.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if mgr == nil || !mgr.Attached() {
		return
	}
	// detach 使用独立的超时上下文，避免因主 ctx 已取消而无法执行
	ctx, c := context.WithTimeout(context.Background(), 20*time.Second)
	defer c()
	if err := mgr.Detach(ctx); err != nil {
		logx.Errorf("usbip detach 失败: %v", err)
	}
}

// applyDeviceIdentity 把 NUT 上报的真实型号/厂商/序列号同步到模拟设备。
// 型号完全来自 device.model（无手动填写），厂商/序列号来自 device.mfr/device.serial。
//
// 返回值表示"是否需要重新枚举"，且**仅当型号发生变化**才为真：型号是飞牛识别
// UPS 的关键显示标识，必须重新挂载才会刷新；厂商/序列号只做静默更新。
//
// 为什么收紧到只看型号：每摘除并重新挂载一次，虚拟设备都会从内核短暂消失，
// 飞牛必须等下一轮轮询才能重新发现它。把重挂载次数压到最少（仅型号变化触发，
// 一次），设备挂载后才能保持稳定存在。
//
// 另：NUT 某一轮数据缺失（mfr / serial 为空）时不再覆盖已有值——否则会把已确定
// 的身份清空，下一轮又被判成"身份变化"，从而反复摘挂。
func (b *Bridge) applyDeviceIdentity(snap ups.Snapshot) bool {
	needReenum := false

	if snap.Model != "" && b.dev.Product() != snap.Model {
		b.dev.SetProduct(snap.Model)
		needReenum = true
	}
	if snap.Mfr != "" && b.dev.Manufacturer() != snap.Mfr {
		b.dev.SetManufacturer(snap.Mfr)
	}
	if snap.Serial != "" && b.dev.Serial() != snap.Serial {
		b.dev.SetSerial(snap.Serial)
	}
	return needReenum
}

// notifyReport 在 NUT 每轮轮询成功后调用，负责同步设备身份（型号/厂商/序列号）。
func (b *Bridge) notifyReport() {
	b.mu.Lock()
	cfg := b.cfg
	b.mu.Unlock()

	// NUT 拿到数据后，同步真实型号/厂商/序列号到模拟设备（杜绝硬编码显示名）。
	// 当身份字段变化且设备已挂载时，摘除并重新挂载以强制 fnOS 重新枚举，否则 fnOS 不刷新。
	snap := b.state.Get()
	if snap.Model != "" || snap.Mfr != "" {
		if b.applyDeviceIdentity(snap) {
			b.mu.Lock()
			attached := b.mountMgr != nil && b.mountMgr.Attached()
			rootCtx := b.rootCtx
			b.mu.Unlock()
			if attached {
				logx.Infof("UPS 身份更新（型号 %q / 厂商 %q），执行多次摘挂重枚举以刷新显示名", snap.Model, snap.Mfr)
				go b.reenumerate(rootCtx, cfg, reenumCycles)
			} else {
				logx.Infof("检测到 UPS 身份（型号 %q / 厂商 %q），已应用，待挂载时生效", snap.Model, snap.Mfr)
			}
		}
	}
}

// reenumerate 反复摘除并重新挂载虚拟 USB 设备若干次，强制 fnOS 重新枚举。
//
// 为什么需要多次：飞牛按 VID:PID(0764:0501) 缓存 UPS 显示名，单次 USB/IP 摘挂不一定
// 能命中其重枚举时机；用「detach → 立即 reattach × N」的循环提高刷新成功率。每次重挂
// 都携带最新真实型号，命中飞牛重枚举时机即可立即显示真名，免去人工反复开关。
//
// 内部每次循环先 stopAutoMount（摘除并取消旧挂载），再解析校验目标并立即 attach（不走
// AttachAfterDelay），随后留 reenumPause 间隔供飞牛重新枚举；ctx 取消则提前结束。
func (b *Bridge) reenumerate(ctx context.Context, cfg config.Config, cycles int) {
	if !cfg.AutoMount {
		logx.Infof("重枚举跳过：自动挂载已关闭")
		return
	}
	for i := 0; i < cycles; i++ {
		b.stopAutoMount() // 摘除当前设备并取消旧挂载
		target, err := config.ParseMountTarget(cfg.AutoMountTarget, cfg.ServerPort)
		if err == nil {
			err = target.Validate()
		}
		if err != nil {
			logx.Errorf("重枚举第 %d/%d 次：挂载目标无效，中止重枚举: %v", i+1, cycles, err)
			return
		}
		mgr := mount.NewManager(target)
		actx, cancel := context.WithCancel(ctx)
		b.mu.Lock()
		b.mountMgr = mgr
		b.mountCtl = cancel
		b.mu.Unlock()
		if err := mgr.Attach(actx); err != nil {
			logx.Errorf("重枚举第 %d/%d 次：挂载失败: %v", i+1, cycles, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(reenumPause):
		}
	}
	logx.Infof("重枚举完成（共 %d 次），当前设备型号 %q", cycles, b.dev.Product())
}

// ---------------------------------------------------------------------------
// httpapi.ConfigController / StatusProvider 实现
// ---------------------------------------------------------------------------

// CurrentConfig 返回当前生效的配置。
func (b *Bridge) CurrentConfig() config.Config {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg
}

// APIToken 返回当前生效的 API token，供同源的 Web UI 静默获取并自动填入写操作请求头。
//
// 为什么不再做"一次性下发"：
//
//	应用改为飞牛反代入口（后端仅监听回环，不再对外暴露端口），所有请求
//	都经飞牛 Nginx 进来，未登录用户根本到不了这里——安全性由飞牛登录会话保证，
//	无需再靠限制下发次数来收敛暴露面。
//	而一次性下发有个真实缺陷：用户换浏览器、换设备或清空 localStorage 后就再也
//	拿不到 token，保存配置会一直 401，只能重装或 SSH。故改为可重复获取。
func (b *Bridge) APIToken() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg.APIToken
}

// ApplyConfig 保存配置并按变化项热重载对应子系统，返回被重载的组件名。
func (b *Bridge) ApplyConfig(newCfg config.Config) ([]string, error) {
	b.mu.Lock()
	old := b.cfg
	rootCtx := b.rootCtx
	b.mu.Unlock()

	if rootCtx == nil {
		return nil, fmt.Errorf("服务尚未就绪，无法应用配置")
	}

	// 密码脱敏回填：占位符或未修改的空密码沿用旧密码，避免把占位符写回配置。
	newCfg.PreservePassword(old)

	// 先落盘，失败则不改变运行状态
	if err := newCfg.Save(b.cfgPath); err != nil {
		return nil, fmt.Errorf("保存配置文件失败: %w", err)
	}
	logx.Infof("配置已写入 %s", b.cfgPath)
	// 同步刷新 var 备份：升级还原的可靠来源（详见 config.MirrorBackup）。
	if err := config.MirrorBackup(newCfg, b.cfgPath); err != nil {
		logx.Warnf("镜像配置备份失败（升级时将无法还原配置）: %v", err)
	}

	b.mu.Lock()
	b.cfg = newCfg
	b.mu.Unlock()

	var changed []string

	nutChanged := old.RemoteUPS != newCfg.RemoteUPS ||
		old.NUTUser != newCfg.NUTUser ||
		old.NUTPass != newCfg.NUTPass
	// USB/IP 端口或绑定地址变化都需重启 USB/IP 子系统（startUSBIP 同时用端口与 bind）
	usbChanged := old.ServerPort != newCfg.ServerPort || old.ServerBind != newCfg.ServerBind
	mountChanged := usbChanged ||
		old.AutoMount != newCfg.AutoMount ||
		old.AutoMountTarget != newCfg.AutoMountTarget
	// Web 端口或绑定地址变化都需重启 Web 子系统（startHTTP 同时用端口与 bind）
	httpChanged := old.HTTPPort != newCfg.HTTPPort || old.HTTPBind != newCfg.HTTPBind

	// USB/IP 端口/绑定变化：先摘挂载，再换端口重启，最后重新挂载
	if usbChanged || mountChanged {
		b.stopAutoMount()
	}
	if usbChanged {
		logx.Infof("USB/IP 端口/绑定 %s:%d -> %s:%d，重启 USB/IP 服务",
			old.ServerBind, old.ServerPort, newCfg.ServerBind, newCfg.ServerPort)
		b.stopUSBIP()
		if err := b.startUSBIP(rootCtx, newCfg); err != nil {
			// 回滚到旧配置，尽力恢复服务可用性
			logx.Errorf("以新绑定 %s:%d 启动 USB/IP 失败: %v，回滚到 %s:%d",
				newCfg.ServerBind, newCfg.ServerPort, err, old.ServerBind, old.ServerPort)
			if rbErr := b.startUSBIP(rootCtx, old); rbErr != nil {
				logx.Errorf("回滚 USB/IP 服务同样失败: %v", rbErr)
			}
			b.mu.Lock()
			b.cfg = old
			b.mu.Unlock()
			_ = old.Save(b.cfgPath)
			return nil, fmt.Errorf("USB/IP %s:%d 无法监听: %w（已回滚配置）", newCfg.ServerBind, newCfg.ServerPort, err)
		}
		changed = append(changed, "USB/IP 服务")
	}

	if nutChanged {
		logx.Infof("NUT 地址 %q -> %q，重启轮询", old.RemoteUPS, newCfg.RemoteUPS)
		b.stopNUT()
		b.startNUT(rootCtx, newCfg)
		changed = append(changed, "NUT 轮询")
	}

	if usbChanged || mountChanged {
		b.startAutoMount(rootCtx, newCfg)
		if newCfg.AutoMount {
			changed = append(changed, "自动挂载")
		} else if old.AutoMount {
			changed = append(changed, "自动挂载（已关闭）")
		}
	}

	if httpChanged {
		logx.Infof("Web 端口/绑定 %s:%d -> %s:%d，将在响应返回后重启 Web 服务",
			old.HTTPBind, old.HTTPPort, newCfg.HTTPBind, newCfg.HTTPPort)
		changed = append(changed, fmt.Sprintf("Web 服务（将切换到 %s:%d，请重新打开页面）", newCfg.HTTPBind, newCfg.HTTPPort))
		go func() {
			time.Sleep(500 * time.Millisecond)
			b.stopHTTP()
			if err := b.startHTTP(rootCtx, newCfg); err != nil {
				logx.Errorf("以新绑定 %s:%d 启动 Web 服务失败: %v，尝试回退到 %s:%d",
					newCfg.HTTPBind, newCfg.HTTPPort, err, old.HTTPBind, old.HTTPPort)
				if rbErr := b.startHTTP(rootCtx, old); rbErr != nil {
					logx.Errorf("回退 Web 服务失败: %v", rbErr)
				}
			}
		}()
	}

	return changed, nil
}

// StatusJSON 组装 /api/status 的响应体。
func (b *Bridge) StatusJSON() any {
	b.mu.Lock()
	srv := b.usbipSrv
	mgr := b.mountMgr
	b.mu.Unlock()

	snap := b.state.Get()
	mounted := mgr != nil && mgr.Attached()

	return map[string]any{
		"service": map[string]any{
			"version":        Version,
			"pid":            os.Getpid(),
			"uptime_seconds": int(time.Since(b.startedAt).Seconds()),
		},
		"nut": map[string]any{
			"connected":  snap.Connected,
			"last_error": snap.LastError,
		},
		"usbip": map[string]any{
			"listening": srv != nil,
		},
		// mount 反映「虚拟 USB 设备是否已挂到本机 vhci」——这是飞牛能否在系统设置里
		// 看到 UPS 的前提。把它暴露给前端，用来引导用户「等设备挂好再去飞牛设置页」，
		// 避免其提前开关导致飞牛缓存"无 UPS"。
		"mount": map[string]any{
			"attached": mounted,
			"model":    snap.Model,
		},
		"ups": map[string]any{
			"model":           snap.Model,
			"input_voltage":   snap.InputVoltage,
			"output_voltage":  snap.OutputVoltage,
			"battery_voltage": snap.BatteryVoltage,
			"battery_charge":  snap.BatteryCharge,
			"battery_runtime": snap.RuntimeSeconds,
			"input_frequency": snap.InputFrequency,
			"status":          snap.StatusRaw,
		},
	}
}

// DefaultUIDir 依据可执行文件位置推断 Web 静态资源目录，避免硬编码路径。
// fpk 安装后二进制位于 TRIM_APPDEST/UPS-Helper，UI 位于 TRIM_APPDEST/ui。
func DefaultUIDir() string {
	candidates := []string{}
	// 首选飞牛官方拓扑：ui/ 固定在应用安装目录 TRIM_APPDEST 之下，与
	// cmd/install_init 中 CGI 的路径（${TRIM_APPDEST}/ui/index.cgi）保持一致。
	// 不能只靠 os.Executable() 推导：从 cmd/main 启动时其所在目录是
	// /var/apps/<app>/cmd，同级并没有 ui/，会误判为"UI 目录不可用"，
	// 使 Web 页面直接返回 {"error":"UI 目录不可用"}。
	if dest := strings.TrimSpace(os.Getenv("TRIM_APPDEST")); dest != "" {
		candidates = append(candidates, filepath.Join(dest, "ui"))
	}
	if appName := strings.TrimSpace(os.Getenv("TRIM_APPNAME")); appName != "" {
		candidates = append(candidates, filepath.Join("/var/apps", appName, "ui"))
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, "ui"), filepath.Join(dir, "app", "ui"))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "ui"), filepath.Join(wd, "app", "ui"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "index.html")); err == nil && !st.IsDir() {
			return c
		}
	}
	logx.Warnf("未找到 Web UI 目录（已尝试: %v），将仅提供 REST API", candidates)
	return ""
}
