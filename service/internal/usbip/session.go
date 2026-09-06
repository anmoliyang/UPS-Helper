package usbip

import (
	"net"
	"sync"
)

// session 表示一条已完成 import、进入 URB 阶段的连接。
//
// EP1 IN 中断端点始终 NAK（见 server.handleSubmit），所有 HID 数据经控制端点
// GET_REPORT 提供，故 session 只需串行化写入与连接关闭，无需维护中断 URB 队列。
type session struct {
	conn net.Conn

	writeMu sync.Mutex

	mu     sync.Mutex
	closed bool
}

func newSession(conn net.Conn) *session {
	return &session{conn: conn}
}

// write 串行化写入，避免控制回复交错。
func (s *session) write(b []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.conn.Write(b)
	return err
}

func (s *session) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.conn.Close()
}
