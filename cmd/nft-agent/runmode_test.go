package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nft-forward/internal/daemon"
)

// 运行形态由命令行参数决定一次。四种参数组合各对应一种结果，其中“两个都给”是
// 配置错误：一个 daemon 至多归一个面板管。
func TestRunModeFromFlags(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "panel.token")
	if err := os.WriteFile(tokenFile, []byte("  tok-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const url = "wss://panel.example/v1/agents"

	mode, err := runModeFromFlags("", tokenFile, false, false, "/run/panel.sock")
	if err != nil || mode != daemon.Standalone() || mode.Managed() {
		t.Errorf("什么都没给应是单机，得到 %+v, %v", mode, err)
	}

	mode, err = runModeFromFlags(url, tokenFile, false, false, "/run/panel.sock")
	if err != nil || mode != daemon.RemotePanel(url, "tok-1") {
		t.Errorf("--connect 应是远端面板托管、token 取自文件并去掉空白，得到 %+v, %v", mode, err)
	}

	// 面板机本机不读 token 文件：文件不存在也不影响。
	mode, err = runModeFromFlags("", filepath.Join(dir, "missing"), false, true, "/run/panel.sock")
	if err != nil || mode != daemon.LocalPanel("/run/panel.sock") {
		t.Errorf("--server-local 应是面板机本机，得到 %+v, %v", mode, err)
	}

	if _, err = runModeFromFlags(url, tokenFile, false, true, "/run/panel.sock"); err == nil || !strings.Contains(err.Error(), "不能同时指定") {
		t.Errorf("--connect 与 --server-local 同时给应报错，得到 %v", err)
	}
}

// 远端面板托管需要一个可用的地址和一个非空的 token，缺哪个都不能退成别的形态。
func TestRunModeFromFlagsRejectsAnUnusableRemotePanel(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "panel.token")
	empty := filepath.Join(dir, "empty.token")
	if err := os.WriteFile(good, []byte("tok-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, url, tokenFile, want string
	}{
		{"明文地址", "ws://panel.example/v1/agents", good, "wss://"},
		{"token 文件不存在", "wss://panel.example/v1/agents", filepath.Join(dir, "missing"), "读取 panel token"},
		{"token 文件为空", "wss://panel.example/v1/agents", empty, "panel token 文件为空"},
	}
	for _, c := range cases {
		mode, err := runModeFromFlags(c.url, c.tokenFile, false, false, "")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应报含 %q 的错误，得到 %+v, %v", c.name, c.want, mode, err)
		}
	}
}
