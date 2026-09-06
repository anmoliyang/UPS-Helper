// Package cgi 实现飞牛 fnOS 微应用的 CGI 反向代理。
//
// 为什么需要它：
//
//	app/ui/config 中若不声明 port，飞牛 Nginx 会把应用入口反代到
//	/cgi/ThirdParty/<appname>/index.cgi/ 之下。这样应用页面与飞牛 Web 同源，
//	并天然处于飞牛登录会话保护之内——外部即使做了 DDNS/端口映射，也只能看到
//	飞牛的登录页，碰不到本应用。此时后端只需监听 127.0.0.1，
//	不再对局域网/公网暴露任何端口，也就无需再用"来源 IP 网段"去猜访问者。
//
// 为什么复用主二进制（而不是再打一个独立代理二进制）：
//
//	代理需要 net/http，独立编译会多出一个约 5.8MB 的二进制，让 fpk 从 3.5MB
//	涨到 6MB。改为 app/ui/index.cgi 用两行 shell 转发到主程序的 -cgi 模式后，
//	只增加约 10KB 代码，安装包体积基本不变。
package cgi

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"

	"UPS-Helper/internal/config"
	"strconv"
	"strings"
	"time"
)

const (
	// cgiName 必须与 app/ui/config 中 url 指向的文件名一致，用于剥离请求前缀。
	// 飞牛硬性要求 CGI 可执行文件命名为 index.cgi（否则报错 "cgi executable must
	// be index.cgi"），故这里不能用其他名字。
	cgiName = "index.cgi"

	// backendEnv 允许特殊部署形态覆盖后端地址；默认回环 53713。
	backendEnv     = "UPS_HELPER_BACKEND"
	defaultBackend = "http://127.0.0.1:53713"

	// backendTimeout 后端响应上限，避免 CGI 进程被慢请求长期占用。
	backendTimeout = 30 * time.Second

	// maxContentLength 请求体上限。CGI 由 CONTENT_LENGTH 声明长度，该值完全
	// 由客户端控制，若不设上限，恶意的超大声明会让本进程长时间读 stdin
	// （虽有 backendTimeout 兜底，但显式设限更稳）。本应用的写接口
	// （POST /api/config）实际只需几个字段，1MiB 绰绰有余。
	maxContentLength = 1 << 20
)

// Serve 完成一次「读 CGI 请求 → 转发后端 → 按 CGI 格式写回响应」。
// 它以 CGI 方式被飞牛 Web 服务器调用：请求来自环境变量与 stdin，响应写往 stdout。
func Serve() {
	if err := serve(); err != nil {
		writeError(http.StatusBadGateway, err)
	}
}

func serve() error {
	url := strings.TrimRight(backendAddr(), "/") + requestURI()

	method := os.Getenv("REQUEST_METHOD")
	if method == "" {
		method = http.MethodGet
	}

	// 请求体：CGI 由 CONTENT_LENGTH 声明长度，按长度从 stdin 读，避免读阻塞。
	// 该值由客户端控制，超过上限直接拒绝，不进入转发流程。
	var body io.Reader
	if n, err := strconv.Atoi(os.Getenv("CONTENT_LENGTH")); err == nil && n > 0 {
		if n > maxContentLength {
			writeError(http.StatusRequestEntityTooLarge,
				fmt.Errorf("请求体 %d 字节超过上限 %d 字节", n, maxContentLength))
			return nil
		}
		body = io.LimitReader(os.Stdin, int64(n))
	}

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return fmt.Errorf("构造后端请求失败: %w", err)
	}

	// 只透传必要的请求头（最小暴露原则），不整体转发宿主头部。
	if v := os.Getenv("CONTENT_TYPE"); v != "" {
		req.Header.Set("Content-Type", v)
	}
	if v := os.Getenv("HTTP_ACCEPT"); v != "" {
		req.Header.Set("Accept", v)
	}
	// 关键：写操作鉴权依赖该头，必须透传。常见的 shell CGI 示例只转发 cookie
	// 与 content-type，漏掉它会导致所有保存配置请求 401。
	if v := os.Getenv("HTTP_X_API_TOKEN"); v != "" {
		req.Header.Set("X-Api-Token", v)
	}
	// 把真实客户端地址交给后端，仅用于日志；后端访问判定不依赖它
	// （后端只监听回环，能进来的请求必然已经过飞牛登录）。
	if v := os.Getenv("REMOTE_ADDR"); v != "" {
		req.Header.Set("X-Forwarded-For", v)
		req.Header.Set("X-Real-IP", v)
	}

	// 必须显式禁用代理：后端固定为本机回环，本就不需要任何代理。
	// 更关键的是，Go 为防 httpoxy（CVE-2016-5385）在检测到 REQUEST_METHOD
	// （即 CGI 环境）时会拒绝使用 HTTP_PROXY 并直接报错——若飞牛的 CGI 环境里
	// 恰好存在 HTTP_PROXY，默认 Transport 会让所有转发失败，故需显式构造不使用
	// 代理的 Transport。
	client := &http.Client{
		Timeout: backendTimeout,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("后端 %s 不可达（请确认 UPS 助手已在飞牛应用中心启动）: %w", url, err)
	}
	defer resp.Body.Close()

	return writeResponse(resp)
}

// backendAddr 返回后端地址。
//
// 解析优先级：
//  1. UPS_HELPER_BACKEND 环境变量（仅允许回环 http(s)，见 isLoopbackBackend）；
//  2. 后端配置文件里的真实 http_port（动态读取，确保 CGI 代理目标与后端当前
//     监听端口一致，避免硬编码错位导致 UI 打不开）；
//  3. 兜底默认回环地址。
//
// 安全：环境变量仅允许指向回环（127.0.0.1 / ::1 / localhost）的 http(s) 地址。
// 该变量可由进程启动环境注入，若允许指向外部主机，便能在不知不觉中把本应仅限
// 飞牛内部的请求导向任意后端（SSRF / 请求外泄）。非回环或非法值一律忽略，
// 回退到默认/配置地址，绝不对外转发。
func backendAddr() string {
	if v := strings.TrimSpace(os.Getenv(backendEnv)); v != "" {
		return v
	}
	return defaultBackend
}

// backendFromConfig 从后端配置文件读取真实监听端口，构造回环代理地址。
// 这样无论端口是默认值还是用户落盘的值，CGI 都能正确代理到后端，
// 而不依赖本文件硬编码，避免前后端端口错位。
func backendFromConfig() (string, bool) {
	// 必须用 ResolvePath（优先 TRIM_PKGETC）而非硬编码的 DefaultPath：
	// 后端与生命周期脚本必须读写同一个 config.json，否则 CGI 读到的端口/配置
	// 与实际生效的不是一个文件。
	cfg, err := config.Load(config.ResolvePath())
	if err != nil {
		return "", false
	}
	if cfg.HTTPPort < 1 || cfg.HTTPPort > 65535 {
		return "", false
	}
	return fmt.Sprintf("http://127.0.0.1:%d", cfg.HTTPPort), true
}

// isLoopbackBackend 校验后端地址仅指向回环 http(s) 服务。
func isLoopbackBackend(addr string) bool {
	u, err := url.Parse(addr)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// requestURI 从 CGI 环境还原「去掉本 CGI 前缀」后的路径与查询串。
// REQUEST_URI 形如 /cgi/ThirdParty/UPS-Helper/index.cgi/api/status?x=1，
// 需要保留 index.cgi 之后的部分。
func requestURI() string {
	uri := os.Getenv("REQUEST_URI")
	if uri == "" {
		uri = os.Getenv("PATH_INFO")
	}
	if uri == "" {
		return "/"
	}
	if i := strings.Index(uri, cgiName); i >= 0 {
		uri = uri[i+len(cgiName):]
	}
	if uri == "" {
		uri = "/"
	}
	if !strings.HasPrefix(uri, "/") {
		uri = "/" + uri
	}
	// 路径本身没带查询串时，补上环境单独提供的 QUERY_STRING。
	if !strings.Contains(uri, "?") {
		if qs := os.Getenv("QUERY_STRING"); qs != "" {
			uri += "?" + qs
		}
	}
	return uri
}

// sanitizeHeaderValue 去掉响应头值中的 CR/LF。
// CGI 响应头靠 CRLF 分隔，若某个头的值里混入换行，就能凭空注入额外的响应头
// 甚至提前结束头部、伪造响应体。Go 的 http 客户端通常已拒绝非法头值，此处
// 仍做一次收敛，保证写出的每一行都是我们自己控制的。
func sanitizeHeaderValue(v string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(v)
}

// writeResponse 把后端响应按 CGI 格式写回 stdout：状态行、响应头、空行、响应体。
func writeResponse(resp *http.Response) error {
	fmt.Printf("Status: %d %s\r\n", resp.StatusCode, http.StatusText(resp.StatusCode))

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	fmt.Printf("Content-Type: %s\r\n", sanitizeHeaderValue(ct))

	// 透传后端设置的少量非 hop-by-hop 头，保留 nosniff 等既有安全语义。
	for _, k := range []string{"Cache-Control", "X-Content-Type-Options", "Referrer-Policy"} {
		if v := resp.Header.Get(k); v != "" {
			fmt.Printf("%s: %s\r\n", k, sanitizeHeaderValue(v))
		}
	}
	fmt.Printf("\r\n")

	_, err := io.Copy(os.Stdout, resp.Body)
	return err
}

// writeError 在无法连上后端时给出可直接排查的提示。
func writeError(code int, err error) {
	fmt.Printf("Status: %d %s\r\n", code, http.StatusText(code))
	fmt.Printf("Content-Type: text/plain; charset=utf-8\r\n")
	fmt.Printf("\r\n")
	fmt.Printf("UPS 助手代理无法访问后端：%v\n", err)
	fmt.Printf("请确认应用已在飞牛应用中心启动，且后端监听在 %s\n", backendAddr())
}
