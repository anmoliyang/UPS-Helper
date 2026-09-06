// Package nut 实现 NUT（Network UPS Tools）网络协议客户端，只用到 LIST VAR。
package nut

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"UPS-Helper/internal/logx"
)

const (
	dialTimeout = 5 * time.Second
	ioTimeout   = 5 * time.Second
	maxVarLines = 4096
)

// Client 是一个到 NUT 服务器的长连接客户端，出错时由调用方重连。
type Client struct {
	addr    string
	upsName string
	conn    net.Conn
	reader  *bufio.Reader
}

// Dial 连接 NUT 服务器并完成可选认证（默认端口 3493 由调用方拼接到 addr 中）。
// user/pass 留空表示匿名连接；飞牛 fnOS 的 upsd 默认要求 monuser/trim-secret 认证。
func Dial(addr, upsName, user, pass string) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("连接 NUT 服务器 %s 失败: %w", addr, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}
	c := &Client{
		addr:    addr,
		upsName: upsName,
		conn:    conn,
		reader:  bufio.NewReaderSize(conn, 8192),
	}
	if user != "" || pass != "" {
		logx.Debugf("nut: 连接 %s，准备认证 (user=%q)", addr, user)
		if user != "" {
			if err := c.auth("USERNAME", user); err != nil {
				conn.Close()
				return nil, err
			}
		}
		if pass != "" {
			if err := c.auth("PASSWORD", pass); err != nil {
				conn.Close()
				return nil, err
			}
		}
	}
	logx.Debugf("nut: 已连接 %s (ups=%s)", addr, upsName)
	return c, nil
}

// auth 发送 NUT 认证握手命令（USERNAME / PASSWORD）并校验响应。
func (c *Client) auth(cmd, val string) error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(ioTimeout)); err != nil {
		return err
	}
	if _, err := c.conn.Write([]byte(fmt.Sprintf("%s %s\n", cmd, val))); err != nil {
		return fmt.Errorf("发送 %s 失败: %w", cmd, err)
	}
	if err := c.conn.SetReadDeadline(time.Now().Add(ioTimeout)); err != nil {
		return err
	}
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("读取 %s 响应失败: %w", cmd, err)
	}
	line = strings.TrimRight(line, "\r\n")
	logx.Debugf("nut < %s", line)
	if strings.HasPrefix(line, "ERR") {
		return fmt.Errorf("NUT %s 认证失败: %s", cmd, line)
	}
	return nil
}

// Close 发送 LOGOUT 并关闭连接。
func (c *Client) Close() {
	if c == nil || c.conn == nil {
		return
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = c.conn.Write([]byte("LOGOUT\n"))
	_ = c.conn.Close()
	c.conn = nil
}

// ListVars 发送 "LIST VAR <ups>" 并解析所有 VAR 行。
func (c *Client) ListVars() (map[string]string, error) {
	if c.conn == nil {
		return nil, errors.New("nut: 连接已关闭")
	}
	cmd := fmt.Sprintf("LIST VAR %s\n", c.upsName)

	if err := c.conn.SetWriteDeadline(time.Now().Add(ioTimeout)); err != nil {
		return nil, err
	}
	if _, err := c.conn.Write([]byte(cmd)); err != nil {
		return nil, fmt.Errorf("发送 LIST VAR 失败: %w", err)
	}
	if err := c.conn.SetReadDeadline(time.Now().Add(ioTimeout)); err != nil {
		return nil, err
	}

	vars := make(map[string]string, 32)
	beginSeen := false

	for i := 0; i < maxVarLines; i++ {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("读取 NUT 响应失败: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		logx.Debugf("nut < %s", line)

		switch {
		case strings.HasPrefix(line, "ERR "):
			return nil, fmt.Errorf("NUT 返回错误: %s", strings.TrimSpace(line[4:]))

		case strings.HasPrefix(line, "BEGIN LIST VAR"):
			beginSeen = true

		case strings.HasPrefix(line, "END LIST VAR"):
			if !beginSeen && len(vars) == 0 {
				return nil, errors.New("NUT 响应缺少 BEGIN LIST VAR")
			}
			return vars, nil

		case strings.HasPrefix(line, "VAR "):
			name, value, ok := parseVarLine(line)
			if ok {
				vars[name] = value
			}
		}
	}
	return nil, errors.New("NUT 响应行数超出上限，可能协议异常")
}

// parseVarLine 解析形如 `VAR myups battery.charge "100"` 的行。
// 变量名可能包含点号，值使用双引号包裹，内部可能出现 \" 与 \\ 转义。
func parseVarLine(line string) (name, value string, ok bool) {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "VAR "))

	// 跳过 UPS 名称字段
	sp := strings.IndexByte(rest, ' ')
	if sp < 0 {
		return "", "", false
	}
	rest = strings.TrimSpace(rest[sp+1:])

	// 变量名
	sp = strings.IndexByte(rest, ' ')
	if sp < 0 {
		return "", "", false
	}
	name = rest[:sp]
	rest = strings.TrimSpace(rest[sp+1:])
	if name == "" {
		return "", "", false
	}
	return name, unquote(rest), true
}

// unquote 去掉 NUT 值两侧的双引号并处理反斜杠转义。
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '"' {
		return s
	}
	body := s[1:]
	if idx := strings.LastIndexByte(body, '"'); idx >= 0 {
		body = body[:idx]
	}
	var b strings.Builder
	b.Grow(len(body))
	for i := 0; i < len(body); i++ {
		if body[i] == '\\' && i+1 < len(body) {
			i++
			b.WriteByte(body[i])
			continue
		}
		b.WriteByte(body[i])
	}
	return b.String()
}
