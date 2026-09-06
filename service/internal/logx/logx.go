// Package logx 提供极简的分级日志，仅依赖标准库。
package logx

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
)

var debugOn atomic.Bool

func init() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

// EnableFile 把日志同时写入文件，便于用户直接 cat 排查。
//
// 为什么需要：日志默认只写 stdout，被飞牛框架接管后用户取不到（此前反复 cat
// 应用目录下的 app.log 都是 No such file），排障只能靠猜。落盘到应用数据目录后，
// 用户可直接读取真实运行态（NUT 是否连上、设备何时挂载、身份是什么）。
//
// 文件超过 2MB 时轮转一次（只保留一份 .1 历史），避免无限增长占满系统盘。
func EnableFile(path string) error {
	if path == "" {
		return fmt.Errorf("日志文件路径为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建日志目录失败: %w", err)
	}
	rotateLogFile(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	log.SetOutput(io.MultiWriter(os.Stdout, f))
	return nil
}

// rotateLogFile 在日志文件超过 2MB 时把它重命名为 .1，仅保留一份历史。
func rotateLogFile(path string) {
	const maxSize = 2 << 20 // 2MB
	info, err := os.Stat(path)
	if err != nil || info.Size() < maxSize {
		return
	}
	_ = os.Remove(path + ".1")
	_ = os.Rename(path, path+".1")
}

// SetDebug 开启/关闭调试输出（对应命令行 -d）。
func SetDebug(v bool) { debugOn.Store(v) }

func Infof(format string, a ...any)  { log.Printf("[INFO ] "+format, a...) }
func Warnf(format string, a ...any)  { log.Printf("[WARN ] "+format, a...) }
func Errorf(format string, a ...any) { log.Printf("[ERROR] "+format, a...) }

func Debugf(format string, a ...any) {
	if debugOn.Load() {
		log.Printf("[DEBUG] "+format, a...)
	}
}

// Hexf 以十六进制转储字节流，仅在调试模式下输出。
func Hexf(prefix string, b []byte) {
	if !debugOn.Load() {
		return
	}
	const maxDump = 64
	dump := b
	suffix := ""
	if len(dump) > maxDump {
		dump = dump[:maxDump]
		suffix = " ..."
	}
	log.Printf("[DEBUG] %s len=%d [% x%s]", prefix, len(b), dump, suffix)
}

// Fatalf 打印错误并以状态码 1 退出。
func Fatalf(format string, a ...any) {
	log.Printf("[FATAL] "+format, a...)
	os.Exit(1)
}
