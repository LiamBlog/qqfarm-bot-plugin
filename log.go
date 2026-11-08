package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

const logBufferSize = 300

var (
	logMu   sync.Mutex
	logRing []string
)

// logf 统一日志出口：stderr + 内存环形缓冲（供管理端导出）。
func logf(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	fmt.Fprintln(os.Stderr, line)

	logMu.Lock()
	logRing = append(logRing, line)
	if len(logRing) > logBufferSize {
		logRing = logRing[len(logRing)-logBufferSize:]
	}
	logMu.Unlock()
}

// RecentLogs 返回最近 limit 行日志（按时间正序）。
func RecentLogs(limit int) []string {
	logMu.Lock()
	defer logMu.Unlock()
	if limit <= 0 || limit > len(logRing) {
		limit = len(logRing)
	}
	out := make([]string, limit)
	copy(out, logRing[len(logRing)-limit:])
	return out
}
