// Package config 负责 JSON 配置文件的读取、校验、保存与目标地址解析。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// hostRe 允许的主机名格式（标签.标签 或 单个标签，含连字符）。
var hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// credRe NUT 凭据允许字符：字母、数字及 . _ - @ /。
var credRe = regexp.MustCompile(`^[A-Za-z0-9._\-@/]+$`)

// upsNameRe NUT UPS 名称允许字符：字母、数字及 . _ -。
// 拒绝换行、控制字符、空白、@、:、/ 等，防止构造远程 NUT 命令（LIST VAR 注入）。
var upsNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// busIDRe 允许的 USB 总线 ID 格式（1-1、1-1.2、2-1.3.4 等）。
// 该值会以 `--busid <id>` 交给 root 执行的 usbip，因此除格式校验外还要排除
// 以 '-' 开头的字符串——否则会被 usbip 的 getopt 当作选项而非参数（参数注入）。
var busIDRe = regexp.MustCompile(`^[0-9]+-[0-9]+(\.[0-9]+)*$`)

// 默认值。DefaultPath 仅在未通过 -c 指定配置文件时使用。
const (
	DefaultPath       = "/etc/UPS助手/config.json"
	DefaultServerPort = 3240
	// DefaultServerBind 默认仅本机。USB/IP 服务端只是给本机 usbip attach 用的
	// 虚拟设备源，主副飞牛同机部署时不需要对局域网开放；仅当确有跨机 USB/IP
	// （飞牛不在同一台）时才需手动改为 0.0.0.0。
	DefaultServerBind = "127.0.0.1"
	DefaultHTTPPort   = 53713
	// DefaultHTTPBind 必须是回环地址：入口改为飞牛反代模式
	// （app/ui/config 不声明 port，由飞牛 Nginx 经 /cgi/ThirdParty/<app>/index.cgi/
	// 反代转发）。此时应用页面与飞牛 Web 同源，处于飞牛登录会话保护之内，
	// 后端只需对回环提供服务，不再向局域网/公网暴露端口。
	// 若改回 0.0.0.0 会绕过飞牛登录，把写操作接口直接暴露给任何能访问该端口的人。
	DefaultHTTPBind        = "127.0.0.1"
	DefaultAutoMountTarget = "127.0.0.1:3240@1-1"
	DefaultNUTPort         = 3493
	DefaultBusID           = "1-1"
	DefaultMountHost       = "127.0.0.1"
)

// ResolvePath 返回配置文件的真实路径：优先飞牛注入的官方配置目录 TRIM_PKGETC，
// 未注入时回退到固定的 /etc 路径。
//
// 优先 TRIM_PKGETC 的原因：生命周期脚本与应用自身都读写该目录下的 config.json，
// 而它属于飞牛托管目录，卸载时由框架清理。若应用落到硬编码的 /etc/UPS助手/config.json，
// 两者指向不同文件——卸载删除的并非应用实际读取的那份，用户在 UI 填过的 remote_ups
// 会残留、并在重装后被读回。统一到 TRIM_PKGETC 才能保证「卸载删除」与「应用读取」指向同一文件。
func ResolvePath() string {
	if d := strings.TrimSpace(os.Getenv("TRIM_PKGETC")); d != "" {
		return filepath.Join(d, "config.json")
	}
	return DefaultPath
}

// redactedPassword 是 NUT 密码的脱敏占位符，用于对外接口避免泄露凭据。
// 约定：ApplyConfig 收到该占位符或空密码（且用户名不变）时视为"未修改"，保留原密码。
const redactedPassword = "******"

// Config 对应 config.json 的结构。
type Config struct {
	// RemoteUPS 远程 NUT UPS 标识，格式 ups_name@host[:nut_port]
	RemoteUPS string `json:"remote_ups"`
	// ServerPort USB/IP 服务监听端口
	ServerPort int `json:"server_port"`
	// ServerBind USB/IP 服务监听地址，默认 127.0.0.1（仅本机，USB/IP 只给本机
	// usbip attach 用，主副飞牛同机时无需对局域网开放）。仅当确有跨机 USB/IP
	// 时才需手动改为 0.0.0.0。
	ServerBind string `json:"server_bind"`
	// AutoMount 是否在启动后自动执行 usbip attach
	AutoMount bool `json:"auto_mount"`
	// AutoMountTarget 自动挂载目标，格式 host:usbip_port@bus-id
	AutoMountTarget string `json:"auto_mount_target"`
	// HTTPPort Web 管理界面端口
	HTTPPort int `json:"http_port"`
	// HTTPBind Web 管理界面监听地址，默认 127.0.0.1（回环）。
	// 入口由飞牛 Nginx 经 /cgi/ThirdParty/<app>/index.cgi/ 反代，页面与
	// 飞牛 Web 同源并处于登录会话保护之内，后端只需对回环提供服务。
	// 改回 0.0.0.0 会绕过飞牛登录、把写操作接口直接暴露给局域网，禁止。
	HTTPBind string `json:"http_bind"`
	// NUTUser 连接 NUT 服务器所用的用户名（飞牛 fnOS 默认 monuser），留空表示匿名
	NUTUser string `json:"nut_user,omitempty"`
	// NUTPass 连接 NUT 服务器所用的密码（飞牛 fnOS 默认 trim-secret）
	NUTPass string `json:"nut_pass,omitempty"`
	// APIToken 用于 /api/config 写操作认证的令牌。安装/首次启动时自动生成
	// 随机值；绝不下发给客户端，仅用于服务端校验（见 RedactSecrets）。
	APIToken string `json:"api_token,omitempty"`
}

// Default 返回全部字段均为默认值的配置。
func Default() Config {
	return Config{
		RemoteUPS:       "",
		ServerPort:      DefaultServerPort,
		ServerBind:      DefaultServerBind,
		AutoMount:       true,
		AutoMountTarget: DefaultAutoMountTarget,
		HTTPPort:        DefaultHTTPPort,
		HTTPBind:        DefaultHTTPBind,
		NUTUser:         "",
		NUTPass:         "",
	}
}

// Load 从 path 读取配置。缺失的字段会保留默认值。
func Load(path string) (Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	cfg.Normalize()
	return cfg, nil
}

// Save 以原子方式（临时文件 + rename）写入配置文件。
func (c Config) Save(path string) error {
	c.Normalize()
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建配置目录 %s 失败: %w", dir, err)
		}
	}
	buf, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return fmt.Errorf("创建临时配置文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// 配置含 NUT 凭据，收紧权限为仅属主可读写，防止其他本地用户读取密码。
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// MirrorBackup 把当前生效配置镜像到 var 备份目录（TRIM_PKGVAR，升级不清），
// 供升级脚本 upgrade_callback 在 etc（TRIM_PKGETC）被飞牛清空后还原，避免升级
// 丢失用户填写的 remote_ups（UPS 名称@IP）。
//
// 为什么需要：飞牛升级会重置 etc 目录，官方要求在升级脚本里自行完成配置迁移。
// 故由运行中的 app 在每次落盘/加载后持续刷新该备份，只要 app 正常运行过，
// 备份即近期有效配置，升级还原不再单点依赖 upgrade_init 钩子是否执行。
//
// 仅当 remote_ups 非空时才覆盖备份：避免用空配置冲掉一份好备份（例如 etc 被清空后
// 首次启动得到空默认配置，此时若回写空备份，会破坏 upgrade_callback 的还原来源）。
// 备份含 NUT 凭据，权限收紧为仅属主可读写。失败返回错误，由调用方记录日志——
// 备份是升级还原的唯一来源，静默失败会让升级后配置无从还原。
func MirrorBackup(cfg Config, cfgPath string) error {
	if cfg.RemoteUPS == "" {
		return nil
	}
	if strings.TrimSpace(cfgPath) == "" {
		return errors.New("cfgPath 为空")
	}
	dir := strings.TrimSpace(os.Getenv("TRIM_PKGVAR"))
	if dir == "" {
		dir = "/var/lib/UPS-Helper"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建备份目录 %s 失败: %w", dir, err)
	}
	bak := filepath.Join(dir, "UPS-Helper-config.bak")
	if err := copyFileAtomic(cfgPath, bak); err != nil {
		return fmt.Errorf("镜像配置到 %s 失败: %w", bak, err)
	}
	_ = os.Chmod(bak, 0o600)
	return nil
}

// copyFileAtomic 原子地把 src 复制到 dst（临时文件 + rename），用于配置备份，
// 避免升级还原时读到半截文件。
func copyFileAtomic(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".bak-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// RedactSecrets 返回脱敏后的配置副本：非空 NUT 密码替换为占位符、API token 清空，
// 避免经无认证的 /api/config 接口泄露凭据。原配置不受影响。
func (c Config) RedactSecrets() Config {
	if c.NUTPass != "" {
		c.NUTPass = redactedPassword
	}
	c.APIToken = ""
	return c
}

// EnsureAPIToken 若尚未设置 API token 则生成随机值。返回是否发生了生成。
// 用于首次启动时为写操作认证提供默认安全的随机令牌。
// 生成失败时返回错误：宁可启动失败，也绝不落一个可预测的弱令牌——
// 弱令牌会让写操作认证形同虚设（若用时间戳等可预测值兜底，攻击者可枚举）。
func (c *Config) EnsureAPIToken() (bool, error) {
	if c.APIToken != "" {
		return false, nil
	}
	tok, err := generateAPIToken()
	if err != nil {
		return false, err
	}
	c.APIToken = tok
	return true, nil
}

// generateAPIToken 用密码学安全随机源生成 32 字节（256 位）令牌的 hex 编码。
func generateAPIToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成 API token 失败（系统随机源不可用）: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// PreservePassword 在保存配置前回填 NUT 密码：若密码为脱敏占位符，或为
// 空密码但用户名未变（表示用户未修改密码），则沿用旧密码，避免把占位符写回配置。
func (c *Config) PreservePassword(old Config) {
	if c.NUTPass == redactedPassword || (c.NUTUser != "" && c.NUTPass == "" && c.NUTUser == old.NUTUser) {
		c.NUTPass = old.NUTPass
	}
}

// MigrateHTTPBindToLoopback 把非回环的 http_bind 收敛为默认值，返回是否发生了变更。
//
// 为什么需要：入口经飞牛反代，后端只需对回环提供服务。但已落盘的 http_bind 可能曾指向
// 非回环地址，而 Normalize 只填空不覆盖，故需在此显式收敛，避免写操作接口对外暴露。
func (c *Config) MigrateHTTPBindToLoopback() bool {
	switch strings.TrimSpace(c.HTTPBind) {
	case "", DefaultHTTPBind, "::1", "localhost":
		return false
	}
	c.HTTPBind = DefaultHTTPBind
	return true
}

// Normalize 去除空白并补齐缺省字段。
func (c *Config) Normalize() {
	c.RemoteUPS = strings.TrimSpace(c.RemoteUPS)
	c.AutoMountTarget = strings.TrimSpace(c.AutoMountTarget)
	c.NUTUser = strings.TrimSpace(c.NUTUser)
	c.NUTPass = strings.TrimSpace(c.NUTPass)
	if c.ServerPort == 0 {
		c.ServerPort = DefaultServerPort
	}
	if c.ServerBind == "" {
		c.ServerBind = DefaultServerBind
	}
	if c.HTTPPort == 0 {
		c.HTTPPort = DefaultHTTPPort
	}
	if c.HTTPBind == "" {
		c.HTTPBind = DefaultHTTPBind
	}
	if c.AutoMountTarget == "" {
		c.AutoMountTarget = fmt.Sprintf("%s:%d@%s", DefaultMountHost, c.ServerPort, DefaultBusID)
	}
}

// Validate 校验配置是否可用于启动服务。
func (c Config) Validate() error {
	if _, err := ParseRemoteUPS(c.RemoteUPS); err != nil {
		return err
	}
	if err := validatePort("server_port", c.ServerPort); err != nil {
		return err
	}
	if err := validateBind("server_bind", c.ServerBind); err != nil {
		return err
	}
	if err := validateBind("http_bind", c.HTTPBind); err != nil {
		return err
	}
	if err := validatePort("http_port", c.HTTPPort); err != nil {
		return err
	}
	if c.ServerPort == c.HTTPPort {
		return fmt.Errorf("server_port 与 http_port 不能相同 (%d)", c.ServerPort)
	}
	if c.AutoMount {
		t, err := ParseMountTarget(c.AutoMountTarget, c.ServerPort)
		if err != nil {
			return err
		}
		// usbip attach 由 root 执行；若目标主机可被指向任意公网地址，攻击者自建
		// USB/IP 服务端即可让本机挂载一个恶意 HID 设备（按键注入 / BadUSB），
		// 等同于把本机 USB 栈交给对方。故限制为回环或私网地址。
		if err := t.Validate(); err != nil {
			return err
		}
	}
	if err := validateCredential("nut_user", c.NUTUser); err != nil {
		return err
	}
	if err := validateCredential("nut_pass", c.NUTPass); err != nil {
		return err
	}
	return nil
}

func validatePort(name string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s 必须在 1-65535 之间，当前为 %d", name, port)
	}
	return nil
}

// validateBind 校验监听绑定地址：接受 0.0.0.0 / 127.0.0.1 / 具体 IPv4、IPv6
// 或合法主机名。拒绝含路径分隔符、空格等非法字符，防止异常配置。
func validateBind(name, bind string) error {
	bind = strings.TrimSpace(bind)
	if bind == "" {
		return fmt.Errorf("%s 不能为空", name)
	}
	// 仅允许可见 ASCII 主机名/IP 字符，禁止路径或注入相关字符。
	// 冒号（:）不在禁止之列，因为 IPv6 地址与 host:port 需要它。
	for _, r := range bind {
		if r == '/' || r == '\\' || r == ' ' {
			return fmt.Errorf("%s 含有非法字符: %q", name, bind)
		}
	}
	if net.ParseIP(bind) != nil {
		return nil
	}
	// 主机名：字母数字、点、连字符
	if hostRe.MatchString(bind) {
		return nil
	}
	return fmt.Errorf("%s 不是合法 IP 或主机名: %q", name, bind)
}

// validateMountHost 限制自动挂载目标主机只能是回环或私网地址。
//
// 为什么：mount 包以 root 执行 `usbip attach --remote <host>`。host 若不受限，
// 攻击者可诱导配置指向自己控制的 USB/IP 服务端，让本机挂载恶意 HID 设备，
// 进而以 root 身份注入按键（BadUSB）。回环与私网已覆盖正常部署形态
// （副飞牛同机 / 同局域网），公网目标一律拒绝。
func validateMountHost(host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("auto_mount_target 的主机 %q 不是合法 IP 地址（不接受主机名，避免 DNS 劫持指向攻击者的 USB/IP 服务端）", host)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return nil
	}
	return fmt.Errorf("auto_mount_target 的主机 %q 不是回环或私网地址，已拒绝（root 级 usbip attach 不允许指向公网）", host)
}

// validateCredential 对 NUT 用户名/密码做最小白名单校验：仅允许
// 可见 ASCII 中较安全的子集，拒绝引号、空格、控制字符等，降低协议层异常风险。
func validateCredential(name, v string) error {
	// 脱敏占位符由 GET /api/config 原样返回，语义是"未修改"（ApplyConfig 会
	// 用旧密码回填）。credRe 不接受 '*'，若不跳过，任何"读回原样提交"的调用方
	// 都会收到 400——API 契约必须自洽。
	if v == "" || v == redactedPassword {
		return nil
	}
	if len(v) > 64 {
		return fmt.Errorf("%s 长度不能超过 64 字符", name)
	}
	if !credRe.MatchString(v) {
		return fmt.Errorf("%s 含有非法字符，仅允许字母、数字及 . _ - @ / 符号", name)
	}
	return nil
}

// UPSTarget 是解析后的 NUT 目标。
type UPSTarget struct {
	Name string
	Host string
	Port int
}

// Addr 返回可直接用于 net.Dial 的地址。
func (t UPSTarget) Addr() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
}

func (t UPSTarget) String() string {
	return fmt.Sprintf("%s@%s:%d", t.Name, t.Host, t.Port)
}

// ParseRemoteUPS 解析 ups_name@host[:nut_port]，支持 [IPv6]:port 写法。
func ParseRemoteUPS(s string) (UPSTarget, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return UPSTarget{}, errors.New("remote_ups 不能为空，格式应为 ups_name@host[:nut_port]")
	}
	at := strings.LastIndex(s, "@")
	if at <= 0 || at >= len(s)-1 {
		return UPSTarget{}, fmt.Errorf("remote_ups 格式错误 %q，应为 ups_name@host[:nut_port]", s)
	}
	name := strings.TrimSpace(s[:at])
	host, port, err := splitHostPort(strings.TrimSpace(s[at+1:]), DefaultNUTPort)
	if err != nil {
		return UPSTarget{}, fmt.Errorf("remote_ups 格式错误 %q: %w", s, err)
	}
	if name == "" {
		return UPSTarget{}, fmt.Errorf("remote_ups 缺少 UPS 名称 %q", s)
	}
	// 白名单校验 UPS 名称：拒绝换行/控制字符/空白/@/: 等，防止其被拼入
	// "LIST VAR <name>" 命令时注入额外的 NUT 协议指令。
	if !upsNameRe.MatchString(name) {
		return UPSTarget{}, fmt.Errorf("remote_ups 的 UPS 名称 %q 含非法字符，仅允许字母、数字及 . _ -", name)
	}
	if host == "" {
		return UPSTarget{}, fmt.Errorf("remote_ups 缺少主机地址 %q", s)
	}
	return UPSTarget{Name: name, Host: host, Port: port}, nil
}

// MountTarget 是解析后的 usbip attach 目标。
type MountTarget struct {
	Host  string
	Port  int
	BusID string
}

func (t MountTarget) String() string {
	return fmt.Sprintf("%s:%d@%s", t.Host, t.Port, t.BusID)
}

// ParseMountTarget 解析 [host][:usbip_port][@bus-id]，缺省 127.0.0.1:<defaultPort>@1-1。
func ParseMountTarget(s string, defaultPort int) (MountTarget, error) {
	if defaultPort < 1 || defaultPort > 65535 {
		defaultPort = DefaultServerPort
	}
	t := MountTarget{Host: DefaultMountHost, Port: defaultPort, BusID: DefaultBusID}

	s = strings.TrimSpace(s)
	if s == "" {
		return t, nil
	}
	hostPart := s
	if at := strings.LastIndex(s, "@"); at >= 0 {
		hostPart = strings.TrimSpace(s[:at])
		if bus := strings.TrimSpace(s[at+1:]); bus != "" {
			t.BusID = bus
		}
	}
	if hostPart == "" {
		return t, nil
	}
	host, port, err := splitHostPort(hostPart, defaultPort)
	if err != nil {
		return t, fmt.Errorf("auto_mount_target 格式错误 %q: %w", s, err)
	}
	if host != "" {
		t.Host = host
	}
	t.Port = port
	if err := t.validateBusID(); err != nil {
		return t, err
	}
	return t, nil
}

// validateBusID 校验 USB 总线 ID 格式。
func (t MountTarget) validateBusID() error {
	if !busIDRe.MatchString(t.BusID) {
		return fmt.Errorf("auto_mount_target 的 bus-id %q 不是合法的 USB 总线 ID（形如 1-1、1-1.2）", t.BusID)
	}
	return nil
}

// Validate 校验挂载目标是否可以被 root 级的 usbip attach 使用。
//
// 为什么需要独立入口：Config.Validate 里的 validateMountHost 只在 auto_mount=true
// 时生效，且 main.go 对校验失败只告警不阻断（保证 UI 可用）。若配置被直接改写、
// 或将来出现不经 Config.Validate 的写入路径，公网目标仍会被 root 挂载。
// 故在真正 attach 之前再拦一次，作为纵深防御。
func (t MountTarget) Validate() error {
	if err := validateMountHost(t.Host); err != nil {
		return err
	}
	return t.validateBusID()
}

// splitHostPort 解析 host、host:port、[ipv6]、[ipv6]:port。
func splitHostPort(s string, defaultPort int) (string, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", defaultPort, nil
	}
	// [IPv6] 或 [IPv6]:port
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", 0, fmt.Errorf("IPv6 地址缺少 ] : %q", s)
		}
		host := s[1:end]
		rest := s[end+1:]
		if rest == "" {
			return host, defaultPort, nil
		}
		if !strings.HasPrefix(rest, ":") {
			return "", 0, fmt.Errorf("无法识别的地址 %q", s)
		}
		port, err := parsePort(rest[1:])
		return host, port, err
	}
	// 裸 IPv6（含多个冒号）视为主机名
	if strings.Count(s, ":") > 1 {
		return s, defaultPort, nil
	}
	if idx := strings.LastIndex(s, ":"); idx >= 0 {
		port, err := parsePort(s[idx+1:])
		if err != nil {
			return "", 0, err
		}
		return s[:idx], port, nil
	}
	return s, defaultPort, nil
}

func parsePort(s string) (int, error) {
	s = strings.TrimSpace(s)
	port, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("端口 %q 不是合法数字", s)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("端口 %d 超出 1-65535", port)
	}
	return port, nil
}
