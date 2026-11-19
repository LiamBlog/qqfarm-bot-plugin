package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type ipcWriteObserver struct{ frames chan []byte }

func (w *ipcWriteObserver) Write(p []byte) (int, error) {
	w.frames <- append([]byte(nil), p...)
	return len(p), nil
}

func readyFrame() map[string]any {
	return map[string]any{"type": "ready", "transport": "native-ipc-v1", "api_version": "1", "framework_api": "3.0.0", "plugin_id": pluginID, "instance_id": "test-instance"}
}

func TestIPCParseFailureClosesConnection(t *testing.T) {
	c := newIPC(strings.NewReader("{invalid}\n"), io.Discard)
	if err := c.run(nil); err == nil {
		t.Fatal("损坏帧应报错")
	}
	select {
	case <-c.closed:
	default:
		t.Fatal("解析错误后必须通知所有请求连接已关闭")
	}
}

func TestIPCCallStopsBeforeReadyOnDisconnect(t *testing.T) {
	c := newIPC(strings.NewReader(""), io.Discard)
	if err := c.run(nil); err != nil {
		t.Fatal(err)
	}
	defer func() {
		select {
		case <-c.ready:
		default:
			close(c.ready)
		}
	}()
	result := make(chan error, 1)
	go func() { _, err := c.call("get_bot_list", nil); result <- err }()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("断连后调用应失败")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("握手前断连不应等待握手超时")
	}
}

func TestIPCCallCleansPendingOnClose(t *testing.T) {
	in, host := io.Pipe()
	defer in.Close()
	defer host.Close()
	out := &ipcWriteObserver{frames: make(chan []byte, 1)}
	c := newIPC(in, out)
	close(c.ready)
	runDone := make(chan error, 1)
	go func() { runDone <- c.run(nil) }()
	result := make(chan error, 1)
	go func() { _, err := c.call("get_bot_list", nil); result <- err }()
	select {
	case <-out.frames:
	case <-time.After(time.Second):
		t.Fatal("未发出请求")
	}
	_ = host.Close()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("读取循环未退出")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("断连应报错")
		}
	case <-time.After(time.Second):
		t.Fatal("请求未随断连退出")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) != 0 {
		t.Fatalf("断连后残留 %d 个请求", len(c.pending))
	}
}

func TestIPCReadyProtocolValidation(t *testing.T) {
	for _, key := range []string{"transport", "api_version", "framework_api", "plugin_id", "instance_id"} {
		t.Run(key, func(t *testing.T) {
			frame := readyFrame()
			frame[key] = ""
			raw, _ := json.Marshal(frame)
			c := newIPC(strings.NewReader(string(raw)+"\n"), io.Discard)
			if err := c.run(nil); err == nil {
				t.Fatal("握手缺少必需字段时应拒绝连接")
			}
			select {
			case <-c.ready:
				t.Fatal("无效握手不能使连接进入就绪状态")
			default:
			}
		})
	}
}

func TestIPCReadyMatchesControlledInstance(t *testing.T) {
	frame := readyFrame()
	raw, _ := json.Marshal(frame)
	c := newIPC(strings.NewReader(string(raw)+"\n"), io.Discard)
	c.expectedInstanceID = "another-instance"
	if err := c.run(nil); err == nil {
		t.Fatal("必须拒绝与受控文件不同的实例")
	}
	select {
	case <-c.ready:
		t.Fatal("实例不符不能进入就绪状态")
	default:
	}
}

func TestIPCCanceledRequestAndLateResponse(t *testing.T) {
	in, host := io.Pipe()
	defer in.Close()
	defer host.Close()
	out := &ipcWriteObserver{frames: make(chan []byte, 1)}
	c := newIPC(in, out)
	close(c.ready)
	runDone := make(chan error, 1)
	go func() { runDone <- c.run(nil) }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := c.callContext(ctx, "get_bot_list", nil); result <- err }()
	var frame map[string]any
	select {
	case raw := <-out.frames:
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("未发出请求")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消错误未保留：%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("取消后仍在等待")
	}
	c.mu.Lock()
	pending := len(c.pending)
	c.mu.Unlock()
	if pending != 0 {
		t.Fatalf("取消后残留 %d 个请求", pending)
	}
	raw, _ := json.Marshal(map[string]any{"type": "action_result", "id": frame["id"], "ok": true, "data": "late"})
	writeDone := make(chan error, 1)
	go func() { _, err := host.Write(append(raw, '\n')); writeDone <- err }()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("迟到响应阻塞读取循环")
	}
	_ = host.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("读取循环未退出")
	}
}

func TestIPCConcurrentCallsKeepProtocolZeroAndMatchResults(t *testing.T) {
	const count = 24
	in, host := io.Pipe()
	defer in.Close()
	defer host.Close()
	out := &ipcWriteObserver{frames: make(chan []byte, count)}
	c := newIPC(in, out)
	close(c.ready)
	done := make(chan error, 1)
	go func() { done <- c.run(nil) }()
	results := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			data, err := c.call("get_qq_classic_farm_code", map[string]any{"protocol_id": 0, "self_id": n})
			if err != nil {
				results <- err
				return
			}
			got, ok := data.(map[string]any)
			if !ok || got["code"] != fmt.Sprintf("TEST-CODE-%d", n) {
				results <- fmt.Errorf("响应匹配错误：%v", data)
				return
			}
			results <- nil
		}(i)
	}
	frames := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		select {
		case raw := <-out.frames:
			var frame map[string]any
			if err := json.Unmarshal(raw, &frame); err != nil {
				t.Fatal(err)
			}
			params, _ := frame["params"].(map[string]any)
			if params["protocol_id"] != float64(0) {
				t.Fatalf("协议 0 丢失：%v", params)
			}
			frames = append(frames, frame)
		case <-time.After(2 * time.Second):
			t.Fatal("并发请求未全部写出")
		}
	}
	for i := len(frames) - 1; i >= 0; i-- {
		frame := frames[i]
		params := frame["params"].(map[string]any)
		raw, _ := json.Marshal(map[string]any{"type": "action_result", "id": frame["id"], "ok": true, "data": map[string]any{"code": fmt.Sprintf("TEST-CODE-%.0f", params["self_id"])}})
		if _, err := host.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	_ = host.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("读取循环未退出")
	}
	wg.Wait()
	for i := 0; i < count; i++ {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) != 0 {
		t.Fatalf("残留 %d 个并发请求", len(c.pending))
	}
}

type ipcShortWriter struct{}

func (ipcShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestIPCShortWriteDetected(t *testing.T) {
	c := newIPC(strings.NewReader(""), ipcShortWriter{})
	close(c.ready)
	if _, err := c.call("get_bot_list", nil); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("必须报告截断的协议帧：%v", err)
	}
	if len(c.pending) != 0 {
		t.Fatal("短写后请求记录未清理")
	}
}

func TestLoadControlledConnection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MENGKA_PLUGIN_DATA_DIR", dir)
	t.Setenv("MENGKA_PLUGIN_CONNECTION_FILE", "")
	if conn, got, err := loadConnection(); err != nil || conn != nil || got != dir {
		t.Fatalf("独立模式兼容错误：%v %q %v", conn, got, err)
	}
	file := filepath.Join(dir, "connection.json")
	t.Setenv("MENGKA_PLUGIN_CONNECTION_FILE", file)
	if _, _, err := loadConnection(); err == nil {
		t.Fatal("指定连接文件不可读时不能降级跳过校验")
	}
	valid := connection{Schema: 1, Transport: "native-ipc-v1", FrameworkAPI: frameworkV, InstanceID: "test-instance", DataDirectory: dir}
	variants := map[string]connection{"valid": valid}
	for _, key := range []string{"schema", "transport", "framework_api", "instance_id", "data_directory"} {
		c := valid
		switch key {
		case "schema":
			c.Schema = 2
		case "transport":
			c.Transport = "http"
		case "framework_api":
			c.FrameworkAPI = "wrong"
		case "instance_id":
			c.InstanceID = "invalid/instance"
		case "data_directory":
			c.DataDirectory = "relative"
		}
		variants[key] = c
	}
	for name, conn := range variants {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(conn)
			if err := os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			got, dataDir, err := loadConnection()
			if name != "valid" {
				if err == nil {
					t.Fatal("无效受控连接文件不能回退为无校验模式")
				}
				return
			}
			if err != nil || got == nil || got.InstanceID != valid.InstanceID || dataDir != dir {
				t.Fatalf("有效文件读取错误：%v %q %v", got, dataDir, err)
			}
		})
	}
	if err := os.WriteFile(file, []byte("{invalid}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadConnection(); err == nil {
		t.Fatal("损坏的连接文件应报错")
	}
}
