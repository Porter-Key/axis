package lspclient

import "sync"

// stderrCapture 限长 LSP stderr 环 (失败排障用)。
//
// 之前直透 os.Stderr: 常驻模式下 gopls 等刷屏灌满 journal, 且 Spawn/initialize
// 失败时无任何服务端日志可查 (排障抓瞎)。现在进程 stderr 进环 (默认 64KB, 只留尾部),
// 失败时把尾巴附在返回错误里; 常驻成功后静默, 零 journal 噪音。
type stderrCapture struct {
	mu  sync.Mutex
	buf []byte
}

// maxStderrCap 环上限 (字节, 超出丢头部留尾部)。
const maxStderrCap = 64 * 1024

func (s *stderrCapture) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	if len(s.buf) > maxStderrCap {
		s.buf = append([]byte(nil), s.buf[len(s.buf)-maxStderrCap:]...)
	}
	return len(p), nil
}

// tail 取尾部 n 字节 (失败错误里附带用)。
func (s *stderrCapture) tail(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buf
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
