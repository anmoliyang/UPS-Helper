// Package httpapi 提供 Web 管理界面的静态文件服务与 REST API。
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"UPS-Helper/internal/config"
	"UPS-Helper/internal/logx"
)

const maxBodyBytes = 64 << 10

// StatusProvider 由应用编排器实现，提供 /api/status 的数据。
type StatusProvider interface {
	StatusJSON() any
}

// ConfigController 由应用编排器实现，负责读取与热重载配置。
type ConfigController interface {
	CurrentConfig() config.Config
	ApplyConfig(cfg config.Config) (changed []string, err error)
	// APIToken 返回当前 API token，供同源的 Web UI 静默获取。
	// 入口由飞牛 Nginx 反代（后端仅监听回环），能到达此处的请求
	// 必然已通过飞牛登录会话校验，故可重复获取而不必限制下发次数。
	APIToken() string
}

// Server 是 Web 管理服务。
type Server struct {
	uiDir  string
	status StatusProvider
	ctl    ConfigController

	srv *http.Server
}

// New 创建 Web 服务。uiDir 为静态文件目录（可为空，此时仅提供 API）。
func New(uiDir string, status StatusProvider, ctl ConfigController) *Server {
	return &Server{uiDir: uiDir, status: status, ctl: ctl}
}

// ListenAndServe 在 addr 上启动 HTTP 服务，阻塞至 ctx 取消或服务出错。
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/token", s.handleToken)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.Handle("/", s.staticHandler())

	s.srv = &http.Server{
		Addr:              addr,
		Handler:           withCommonHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("web 服务监听 %s 失败: %w", addr, err)
	}
	logx.Infof("Web 管理服务已监听 %s (UI 目录: %s)", addr, orNone(s.uiDir))

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()

	if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logx.Infof("Web 管理服务已停止 (%s)", addr)
	return nil
}

// Close 立即关闭 HTTP 服务。
func (s *Server) Close() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
}

func (s *Server) staticHandler() http.Handler {
	if s.uiDir == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "UI 目录不可用"})
		})
	}
	// 解析 uiDir 的符号链接得到真实根路径，作为穿越防护的基准。
	root, err := filepath.EvalSymlinks(s.uiDir)
	if err != nil {
		root = s.uiDir
	}
	fs := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 单页应用：未知路径回落到 index.html
		clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "." || clean == "/" {
			clean = "index.html"
		}
		full := filepath.Join(root, clean)
		// 解析符号链接，防止经 webroot 内的符号链接跳出到外部文件。
		if resolved, e := filepath.EvalSymlinks(full); e == nil {
			full = resolved
		}
		// 路径穿透防护：解析后的目标必须仍位于 root 之内。
		// 若 clean 含 "../" 企图跳出，filepath.Rel 会返回带 ".." 的路径，
		// 此时直接回落 index.html，绝不读取 webroot 之外的文件。
		if rel, err := filepath.Rel(root, full); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			http.ServeFile(w, r, filepath.Join(root, "index.html"))
			return
		}
		if st, err := os.Stat(full); err != nil || st.IsDir() {
			http.ServeFile(w, r, filepath.Join(root, "index.html"))
			return
		}
		fs.ServeHTTP(w, r)
	})
}

// GET /api/status
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 GET"})
		return
	}
	writeJSON(w, http.StatusOK, s.status.StatusJSON())
}

// GET /api/token 返回当前 API token，供同源的 Web UI 静默获取后自动填入写操作请求头。
//
// 安全性来自入口：应用不再对外监听端口，请求只能经飞牛 Nginx 反代进来，
// 未通过飞牛登录的用户到不了这里。因此无需再限制"只能下发一次"；
// 反过来，一次性下发会让换浏览器 / 清空 localStorage 的用户永远拿不到 token。
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 GET"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": s.ctl.APIToken()})
}

// GET /api/config、POST /api/config
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.ctl.CurrentConfig().RedactSecrets())

	case http.MethodPost, http.MethodPut:
		// 写操作认证：要求请求头携带与服务端一致的 API token。
		// Token 缺失或为空一律拒绝（空串必须显式拦掉，否则空对空会误判为通过）。
		expected := s.ctl.CurrentConfig().APIToken
		if expected == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "服务端未配置 API token"})
			return
		}
		// 恒定时间比较，避免通过响应耗时差异逐字节爆破 token。
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Api-Token")), []byte(expected)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "缺少或错误的 API token"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "读取请求体失败: " + err.Error()})
			return
		}
		// 以当前配置为基准，允许仅提交部分字段
		current := s.ctl.CurrentConfig()
		cfg := current
		if err := json.Unmarshal(body, &cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON 解析失败: " + err.Error()})
			return
		}
		// api_token 由服务端生成与管理，忽略请求体中的任何覆盖。
		cfg.APIToken = expected
		// 监听绑定地址不允许经 API 修改："/api/token 免鉴权" 完全建立在
		// "后端只监听回环、入口必经飞牛登录" 这一地基上。若允许把 http_bind 改成
		// 0.0.0.0，持有 token 者即可重启 Web 服务到对外地址，让写操作接口脱离飞牛
		// 登录保护并持久化——等于认证模型可被自行拆除。
		// server_bind 同理：USB/IP 的 OP_REQ_IMPORT 无任何鉴权，对外暴露即等于
		// 把虚拟 USB 设备交给局域网任何人。
		// 确有跨机 USB/IP 需求时，请直接修改配置文件（需 SSH/Shell 权限）。
		cfg.HTTPBind = current.HTTPBind
		cfg.ServerBind = current.ServerBind
		cfg.Normalize()
		if err := cfg.Validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		changed, err := s.ctl.ApplyConfig(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if changed == nil {
			changed = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"changed": changed,
			// 必须脱敏：cfg 含明文 nut_pass 与 api_token。GET 走 RedactSecrets，
			// 之前 POST 漏了，导致持有 token 者可用空 body 换回明文凭据。
			"config":  cfg.RedactSecrets(),
			"message": reloadMessage(changed),
		})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 GET / POST"})
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now().Format(time.RFC3339)})
}

func reloadMessage(changed []string) string {
	if len(changed) == 0 {
		return "配置已保存，无需重载任何组件"
	}
	return "配置已保存并热重载: " + strings.Join(changed, "、")
}

func withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// 飞牛微应用（桌面弹窗）用宿主 iframe 加载本 UI，且本 UI 是单文件内联
		// HTML（<style>/<script> 均为内联）。任何 CSP 头（含 default-src 'self'）
		// 都会按浏览器策略拦截内联样式与脚本，导致 CSS/JS 完全不生效、SDK 握手
		// 超时。因此**严禁设置 Content-Security-Policy / X-Frame-Options**——
		// 飞牛官方文档亦未要求应用自设 CSP，且明确页面由宿主 iframe 加载。
		// 仅保留无副作用的 nosniff 与 Referrer-Policy。
		w.Header().Set("Referrer-Policy", "same-origin")
		// /api/ 一律不缓存（状态必须实时）。
		// HTML 同样不缓存：本 UI 是单文件内联 HTML，一旦被浏览器缓存，
		// 用户不手动强制刷新就仍会运行旧前端、显示旧数值甚至沿用旧请求路径。
		// 图片等静态资源可正常缓存。
		if strings.HasPrefix(r.URL.Path, "/api/") ||
			r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, ".html") {
			w.Header().Set("Cache-Control", "no-store")
		}
		logx.Debugf("http %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		logx.Debugf("写入 JSON 响应失败: %v", err)
	}
}

func orNone(s string) string {
	if s == "" {
		return "<未找到>"
	}
	return s
}
