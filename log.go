package main

import (
	"fmt"
	"os"
	"time"
)

// logf 统一日志出口：stderr，避免污染 stdout 上的 IPC 协议帧。
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}
