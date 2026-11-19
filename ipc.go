package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

const (
	pluginID    = "qqfarm-bot"
	pluginName  = "农场桥接助手"
	frameworkV  = "3.0.0"
	apiVersion  = "1"
	ipcMaxFrame = 16 << 20
)

var version = "3.8.1"
var instanceIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// connection 框架写入的受控连接文件。
type connection struct {
	Schema        int    `json:"schema"`
	Transport     string `json:"transport"`
	FrameworkAPI  string `json:"framework_api"`
	InstanceID    string `json:"instance_id"`
	DataDirectory string `json:"data_directory"`
}

// IPC 负责 stdin/stdout 上的 native-ipc-v1 帧收发；run 只启动一次。
type IPC struct {
	in                 *bufio.Scanner
	out                io.Writer
	wmu                sync.Mutex
	mu                 sync.Mutex
	pending            map[string]chan map[string]any
	ready              chan struct{}
	once               sync.Once
	instID             string
	expectedInstanceID string
	closed             chan struct{}
}

func newIPC(in io.Reader, out io.Writer) *IPC {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), ipcMaxFrame)
	return &IPC{in: sc, out: out, pending: make(map[string]chan map[string]any), ready: make(chan struct{}), closed: make(chan struct{})}
}

func (c *IPC) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	n, err := c.out.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

func (c *IPC) hello() error {
	return c.write(map[string]any{"type": "hello", "api_version": apiVersion, "framework_api": frameworkV, "plugin_id": pluginID, "version": version})
}

func (c *IPC) acceptReady(msg map[string]any) error {
	id, _ := msg["instance_id"].(string)
	if msg["transport"] != "native-ipc-v1" || msg["api_version"] != apiVersion || msg["framework_api"] != frameworkV || msg["plugin_id"] != pluginID || !instanceIDPattern.MatchString(id) {
		return errors.New("IPC ready 协议或插件身份不匹配")
	}
	if c.expectedInstanceID != "" && id != c.expectedInstanceID {
		return errors.New("IPC ready 实例与受控连接文件不匹配")
	}
	select {
	case <-c.ready:
		if c.instID != id {
			return errors.New("IPC ready 实例在连接中途发生变化")
		}
	default:
		c.instID = id
		c.once.Do(func() { close(c.ready) })
	}
	return nil
}

// run 的任何退出路径都必须通知等待者，包括损坏帧、超长帧和底层读错误。
func (c *IPC) run(onEvent func(map[string]any)) error {
	defer close(c.closed)
	for c.in.Scan() {
		line := c.in.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(line, &msg); err != nil {
			return fmt.Errorf("IPC 帧解析失败：%w", err)
		}
		select {
		case <-c.ready:
		default:
			if msg["type"] != "ready" {
				return errors.New("IPC 首帧必须为 ready")
			}
		}
		switch msg["type"] {
		case "ready":
			if err := c.acceptReady(msg); err != nil {
				return err
			}
		case "action_result":
			id, _ := msg["id"].(string)
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
		case "event":
			if onEvent != nil {
				if data, ok := msg["data"].(map[string]any); ok {
					go onEvent(data)
				}
			}
		}
	}
	return c.in.Err()
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// call 发起 action 并等待结果（握手 10 秒、响应 30 秒超时）。
func (c *IPC) call(action string, params map[string]any) (any, error) {
	return c.callContext(context.Background(), action, params)
}

// callContext 取消的是本地等待，不能撤销已经发出的 action；调用方不可据此盲目重放。
func (c *IPC) callContext(ctx context.Context, action string, params map[string]any) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-c.closed:
		return nil, errors.New("IPC 连接已关闭")
	default:
	}
	readyTimer := time.NewTimer(10 * time.Second)
	defer readyTimer.Stop()
	select {
	case <-c.ready:
	case <-c.closed:
		return nil, errors.New("IPC 连接已关闭")
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-readyTimer.C:
		return nil, errors.New("IPC 尚未就绪")
	}
	readyTimer.Stop()
	id := newID()
	ch := make(chan map[string]any, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	select {
	case <-c.closed:
		return nil, errors.New("IPC 连接已关闭")
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if err := c.write(map[string]any{"type": "action", "id": id, "action": action, "params": params}); err != nil {
		return nil, fmt.Errorf("IPC 写入失败：%w", err)
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	decode := func(msg map[string]any) (any, error) {
		if ok, _ := msg["ok"].(bool); !ok {
			text, _ := msg["error"].(string)
			if text == "" {
				text = "action 调用失败"
			}
			return nil, errors.New(text)
		}
		return msg["data"], nil
	}
	select {
	case msg := <-ch:
		return decode(msg)
	case <-timer.C:
		return nil, errors.New("action 调用超时（请求可能已执行）")
	case <-ctx.Done():
		return nil, fmt.Errorf("action 等待已取消（请求可能已执行）：%w", ctx.Err())
	case <-c.closed:
		// 宿主可能在发送最终结果后立即关闭管道，优先保留已经收到的结果。
		select {
		case msg := <-ch:
			return decode(msg)
		default:
			return nil, errors.New("IPC 连接已关闭（请求可能已执行）")
		}
	}
}

func loadConnection() (*connection, string, error) {
	path := os.Getenv("MENGKA_PLUGIN_CONNECTION_FILE")
	dataDir := os.Getenv("MENGKA_PLUGIN_DATA_DIR")
	if path == "" {
		return nil, dataDir, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, dataDir, fmt.Errorf("读取受控连接文件失败：%w", err)
	}
	var conn connection
	if err := json.Unmarshal(raw, &conn); err != nil {
		return nil, dataDir, fmt.Errorf("受控连接文件解析失败：%w", err)
	}
	if conn.Schema != 1 || conn.Transport != "native-ipc-v1" || conn.FrameworkAPI != frameworkV || !instanceIDPattern.MatchString(conn.InstanceID) || !filepath.IsAbs(conn.DataDirectory) {
		return nil, dataDir, errors.New("受控连接文件协议、实例标识或数据目录无效")
	}
	return &conn, conn.DataDirectory, nil
}
