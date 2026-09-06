package usbip

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"UPS-Helper/internal/logx"
)

const (
	// handshakeTimeout 是 op 握手阶段的读超时。
	// 只覆盖握手：连上来不发数据的空连接（slowloris）会在此被踢掉。
	// 导入成功后必须清除该 deadline——USB/IP 会话是长连接，需常驻转发 URB，
	// 若沿用短超时会把正常挂载的设备反复摘掉。
	handshakeTimeout = 10 * time.Second

	// maxConns 并发连接上限。USB/IP 正常只需 1 条连接（本机 usbip attach），
	// 上限用于防止被恶意耗尽连接/协程资源（默认仅监听回环，危害有限；
	// 一旦 server_bind 被改到对外地址，这就是必要的兜底）。
	maxConns = 8
)

// SetupPacket 是解析后的 USB setup packet（各字段按 USB 规范为小端）。
type SetupPacket struct {
	RequestType uint8
	Request     uint8
	Value       uint16
	Index       uint16
	Length      uint16
}

// Device 描述被模拟的 USB 设备需要提供的能力。
type Device interface {
	// Info 返回 USB/IP 设备描述。
	Info() DeviceInfo
	// Control 处理 EP0 控制传输。返回 IN 方向要回给主机的数据与 URB 状态码。
	Control(setup SetupPacket, out []byte) (data []byte, status int32)
}

// Server 是 USB/IP 协议服务端。
type Server struct {
	dev Device

	mu       sync.Mutex
	ln       net.Listener
	sessions map[*session]struct{}
}

// NewServer 创建服务端。
func NewServer(dev Device) *Server {
	return &Server{dev: dev, sessions: make(map[*session]struct{})}
}

// ListenAndServe 在 addr 上监听并阻塞服务，直到 ctx 取消或监听器关闭。
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("USB/IP 监听 %s 失败: %w", addr, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()

	logx.Infof("USB/IP 服务端已监听 %s (busid=%s, %04x:%04x)",
		addr, s.dev.Info().BusID, s.dev.Info().IDVendor, s.dev.Info().IDProduct)

	go func() {
		<-ctx.Done()
		s.Close()
	}()

	// 并发连接闸门：满了就拒绝并立即关闭，避免无上限地起协程。
	connSem := make(chan struct{}, maxConns)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				logx.Infof("USB/IP 服务端已停止监听 %s", addr)
				return nil
			}
			logx.Warnf("USB/IP accept 失败: %v", err)
			continue
		}
		select {
		case connSem <- struct{}{}:
			go func() {
				defer func() { <-connSem }()
				s.handleConn(conn)
			}()
		default:
			logx.Warnf("usbip: 连接数已达上限 %d，拒绝新连接 %s", maxConns, conn.RemoteAddr())
			_ = conn.Close()
		}
	}
}

// Close 关闭监听器与所有会话。
func (s *Server) Close() {
	s.mu.Lock()
	ln := s.ln
	s.ln = nil
	sessions := make([]*session, 0, len(s.sessions))
	for sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, sess := range sessions {
		sess.close()
	}
}

// EP1 IN 中断端点始终 NAK，数据由控制端点 GET_REPORT 主动轮询获取，
// 故无需向客户端推送中断报告。

func (s *Server) addSession(sess *session) {
	s.mu.Lock()
	s.sessions[sess] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) removeSession(sess *session) {
	s.mu.Lock()
	delete(s.sessions, sess)
	s.mu.Unlock()
}

// handleConn 处理一条连接：先走 op 握手阶段，导入成功后进入 URB 阶段。
func (s *Server) handleConn(conn net.Conn) {
	remote := conn.RemoteAddr().String()
	logx.Debugf("usbip: 新连接 %s", remote)
	defer func() {
		if r := recover(); r != nil {
			logx.Errorf("usbip: 处理连接 %s 时 panic: %v", remote, r)
		}
	}()

	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	// 握手阶段限时：防止连上来却一直不发数据的空连接长期占用进程资源。
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	reader := bufio.NewReaderSize(conn, 8192)

	hdr, err := ReadOpHeader(reader)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			logx.Debugf("usbip: 读取 op 头失败 (%s): %v", remote, err)
		}
		_ = conn.Close()
		return
	}
	logx.Debugf("usbip: op 请求 version=0x%04x code=0x%04x status=%d", hdr.Version, hdr.Code, hdr.Status)

	switch hdr.Code {
	case OpReqDevlist:
		s.handleDevlist(conn, remote)
		_ = conn.Close()

	case OpReqImport:
		if !s.handleImport(conn, reader, remote) {
			_ = conn.Close()
			return
		}
		// 导入成功，握手结束：清除读超时。此后是长驻的 URB 转发会话，
		// 不能再受握手超时约束，否则正常挂载的设备会被定时摘掉。
		_ = conn.SetReadDeadline(time.Time{})
		s.serveURB(conn, reader, remote)

	default:
		logx.Warnf("usbip: 不支持的操作码 0x%04x (来自 %s)", hdr.Code, remote)
		_ = conn.Close()
	}
}

// handleDevlist 回复 OP_REQ_DEVLIST (0x8005)。
func (s *Server) handleDevlist(conn net.Conn, remote string) {
	dev := s.dev.Info()
	reply := BuildDevlistReply([]DeviceInfo{dev})
	logx.Debugf("usbip: 回复 OP_REP_DEVLIST 给 %s (%d 字节, 1 个设备)", remote, len(reply))
	logx.Hexf("usbip > OP_REP_DEVLIST", reply)
	if _, err := conn.Write(reply); err != nil {
		logx.Warnf("usbip: 发送设备列表失败: %v", err)
	}
}

// handleImport 处理 OP_REQ_IMPORT (0x8003)，成功返回 true。
func (s *Server) handleImport(conn net.Conn, reader io.Reader, remote string) bool {
	busID, err := ReadBusID(reader)
	if err != nil {
		logx.Warnf("usbip: 读取 import busid 失败: %v", err)
		return false
	}
	dev := s.dev.Info()
	if busID != dev.BusID {
		logx.Warnf("usbip: %s 请求导入未知设备 busid=%q（本机仅提供 %q）", remote, busID, dev.BusID)
		reply := BuildImportReply(dev, 1)
		_, _ = conn.Write(reply)
		return false
	}
	reply := BuildImportReply(dev, 0)
	logx.Hexf("usbip > OP_REP_IMPORT", reply)
	if _, err := conn.Write(reply); err != nil {
		logx.Warnf("usbip: 发送 import 回复失败: %v", err)
		return false
	}
	logx.Infof("USB/IP 设备已被 %s 导入 (busid=%s)", remote, busID)
	return true
}

// serveURB 进入 URB 阶段，循环处理 CMD_SUBMIT / CMD_UNLINK。
func (s *Server) serveURB(conn net.Conn, reader *bufio.Reader, remote string) {
	sess := newSession(conn)
	s.addSession(sess)
	defer func() {
		s.removeSession(sess)
		sess.close()
		logx.Infof("USB/IP 客户端已断开 %s", remote)
	}()

	for {
		hdr, err := ReadURBHeader(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				logx.Debugf("usbip: 读取 URB 头失败 (%s): %v", remote, err)
			}
			return
		}

		switch hdr.Command {
		case CmdSubmit:
			if err := s.handleSubmit(sess, reader, hdr); err != nil {
				logx.Warnf("usbip: 处理 CMD_SUBMIT 失败: %v", err)
				return
			}
		case CmdUnlink:
			s.handleUnlink(sess, hdr)
		default:
			logx.Warnf("usbip: 未知 URB 命令 0x%08x (来自 %s)", hdr.Command, remote)
			return
		}
	}
}

// handleSubmit 处理 USBIP_CMD_SUBMIT (0x0001)。
func (s *Server) handleSubmit(sess *session, reader io.Reader, hdr URBHeader) error {
	if err := ValidateTransferLength(hdr.TransferBufferLength); err != nil {
		return err
	}

	// OUT 方向的数据紧跟在头之后。
	var outData []byte
	if hdr.Direction == DirOut && hdr.TransferBufferLength > 0 {
		outData = make([]byte, hdr.TransferBufferLength)
		if _, err := io.ReadFull(reader, outData); err != nil {
			return fmt.Errorf("读取 OUT 数据失败: %w", err)
		}
	}
	// 等时传输的包描述符（本设备不使用，读掉即可）。
	//
	// 数量越界时不得跳过不读：残留字节会被当成下一个 URB 头解析，使会话
	// 永久失步并读到任意数据。此时协议状态已无法对齐，直接断开让客户端重连，
	// 比带着错乱的流继续解析更安全。
	n := hdr.NumberOfPackets
	if n < 0 || n > maxISOPackets {
		return fmt.Errorf("非法的 number_of_packets: %d（合法范围 0-%d）", n, maxISOPackets)
	}
	if n > 0 {
		if _, err := io.CopyN(io.Discard, reader, int64(n)*isoPacketLen); err != nil {
			return fmt.Errorf("跳过 ISO 包描述符失败: %w", err)
		}
	}

	logx.Debugf("usbip: CMD_SUBMIT seq=%d ep=%d dir=%d len=%d flags=0x%08x",
		hdr.Seqnum, hdr.Endpoint, hdr.Direction, hdr.TransferBufferLength, hdr.TransferFlags)

	switch hdr.Endpoint {
	case 0:
		// EP0 控制传输
		setup := SetupPacket{
			RequestType: hdr.Setup[0],
			Request:     hdr.Setup[1],
			Value:       uint16(hdr.Setup[2]) | uint16(hdr.Setup[3])<<8, // setup packet 为小端
			Index:       uint16(hdr.Setup[4]) | uint16(hdr.Setup[5])<<8,
			Length:      uint16(hdr.Setup[6]) | uint16(hdr.Setup[7])<<8,
		}
		data, status := s.dev.Control(setup, outData)
		if hdr.Direction == DirIn {
			// 不得超过主机申请的长度
			limit := int(hdr.TransferBufferLength)
			if len(data) > limit {
				data = data[:limit]
			}
		} else {
			data = nil
		}
		return sess.write(BuildRetSubmit(hdr.Seqnum, status, data))

	case 1:
		// EP1 IN 中断端点：始终 NAK（返回 0 字节）。所有 HID 数据通过控制端点
		// GET_REPORT（带 Report ID）提供，NUT 的 usbhid-ups 据此读取型号/序列号/电量/电压/状态。
		if hdr.Direction != DirIn {
			return sess.write(BuildRetSubmit(hdr.Seqnum, StatusEPIPE, nil))
		}
		return sess.write(BuildRetSubmit(hdr.Seqnum, StatusOK, nil))

	default:
		logx.Debugf("usbip: 未实现的端点 %d，返回 STALL", hdr.Endpoint)
		return sess.write(BuildRetSubmit(hdr.Seqnum, StatusEPIPE, nil))
	}
}

// handleUnlink 处理 USBIP_CMD_UNLINK (0x0002)。
// EP1 IN 中断端点始终 NAK，无挂起的 URB，故总是回复"目标 URB 已完成，无需取消"。
func (s *Server) handleUnlink(sess *session, hdr URBHeader) {
	status := StatusOK // 0：目标 URB 已完成，无需取消
	logx.Debugf("usbip: CMD_UNLINK seq=%d target=%d -> status=%d", hdr.Seqnum, hdr.UnlinkSeqnum, status)
	if err := sess.write(BuildRetUnlink(hdr.Seqnum, status)); err != nil {
		logx.Debugf("usbip: 发送 RET_UNLINK 失败: %v", err)
	}
}
