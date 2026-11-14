package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

var version = "3.7.4"

// connection 框架写入的受控连接文件。
type connection struct {
	Schema        int    `json:"schema"`
	Transport     string `json:"transport"`
	FrameworkAPI  string `json:"framework_api"`
	InstanceID    string `json:"instance_id"`
	DataDirectory string `json:"data_directory"`
}

// IPC 负责 stdin/stdout 上的 native-ipc-v1 帧收发。
type IPC struct {
	in      *bufio.Scanner
	out     io.Writer
	wmu     sync.Mutex
	mu      sync.Mutex
	pending map[string]chan map[string]any
	ready   chan struct{}
	once    sync.Once
	instID  string
	closed  chan struct{}
}

func newIPC(in io.Reader, out io.Writer) *IPC {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), ipcMaxFrame)
	return &IPC{
		in:      sc,
		out:     out,
		pending: make(map[string]chan map[string]any),
		ready:   make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (c *IPC) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = fmt.Fprintf(c.out, "%s\n", data)
	return err
}

func (c *IPC) hello() error {
	return c.write(map[string]any{
		"type":          "hello",
		"api_version":   apiVersion,
		"framework_api": frameworkV,
		"plugin_id":     pluginID,
		"version":       version,
	})
}

// run 读取帧直到连接关闭；ready 帧到达后关闭 ready channel。
func (c *IPC) run(onEvent func(map[string]any)) error {
	for c.in.Scan() {
		line := c.in.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(line, &msg); err != nil {
			return fmt.Errorf("IPC 帧解析失败：%w", err)
		}
		switch msg["type"] {
		case "ready":
			if id, _ := msg["instance_id"].(string); id != "" {
				c.instID = id
			}
			c.once.Do(func() { close(c.ready) })
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
	close(c.closed)
	return c.in.Err()
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// call 发起 action 并等待结果（30 秒超时）。
func (c *IPC) call(action string, params map[string]any) (any, error) {
	select {
	case <-c.ready:
	case <-time.After(10 * time.Second):
		return nil, errors.New("IPC 尚未就绪")
	}
	id := newID()
	ch := make(chan map[string]any, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(map[string]any{"type": "action", "id": id, "action": action, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("IPC 写入失败：%w", err)
	}
	select {
	case msg := <-ch:
		if ok, _ := msg["ok"].(bool); !ok {
			text, _ := msg["error"].(string)
			if text == "" {
				text = "action 调用失败"
			}
			return nil, errors.New(text)
		}
		return msg["data"], nil
	case <-time.After(30 * time.Second):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, errors.New("action 调用超时")
	case <-c.closed:
		return nil, errors.New("IPC 连接已关闭")
	}
}

func loadConnection() (*connection, string) {
	path := os.Getenv("MENGKA_PLUGIN_CONNECTION_FILE")
	dataDir := os.Getenv("MENGKA_PLUGIN_DATA_DIR")
	if path == "" {
		return nil, dataDir
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		logf("连接文件不可用（%v），以无实例校验模式继续", err)
		return nil, dataDir
	}
	var conn connection
	if err := json.Unmarshal(raw, &conn); err != nil || conn.Schema != 1 || conn.Transport != "native-ipc-v1" {
		logf("连接文件格式不符（%v），以无实例校验模式继续", err)
		return nil, dataDir
	}
	if conn.DataDirectory != "" {
		dataDir = conn.DataDirectory
	}
	return &conn, dataDir
}
