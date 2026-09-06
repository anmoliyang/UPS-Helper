package nut

import (
	"context"
	"time"

	"UPS-Helper/internal/config"
	"UPS-Helper/internal/logx"
	"UPS-Helper/internal/ups"
)

// PollInterval 是 UPS 状态刷新周期（需求：每 2 秒）。
const PollInterval = 2 * time.Second

// Poller 周期性地从 NUT 服务器拉取变量并写入状态缓存。
type Poller struct {
	target config.UPSTarget
	state  *ups.State
	user   string
	pass   string
	// OnUpdate 在每次成功刷新后调用（用于触发 HID 中断推送）。
	OnUpdate func()
}

// NewPoller 创建轮询器。user/pass 为连接 NUT 时的认证凭证（留空表示匿名）。
func NewPoller(target config.UPSTarget, state *ups.State, onUpdate func(), user, pass string) *Poller {
	return &Poller{target: target, state: state, OnUpdate: onUpdate, user: user, pass: pass}
}

// Run 阻塞运行直到 ctx 取消。连接失败时按 PollInterval 自动重试。
func (p *Poller) Run(ctx context.Context) {
	logx.Infof("NUT 轮询启动: %s，间隔 %s", p.target.String(), PollInterval)

	var client *Client
	defer func() {
		if client != nil {
			client.Close()
		}
	}()

	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	poll := func() {
		if client == nil {
			c, err := Dial(p.target.Addr(), p.target.Name, p.user, p.pass)
			if err != nil {
				p.state.SetDisconnected(err.Error())
				logx.Warnf("%v", err)
				return
			}
			client = c
			logx.Infof("已连接 NUT 服务器 %s (UPS: %s)", p.target.Addr(), p.target.Name)
		}
		vars, err := client.ListVars()
		if err != nil {
			logx.Warnf("拉取 UPS 变量失败: %v", err)
			p.state.SetDisconnected(err.Error())
			client.Close()
			client = nil
			return
		}
		p.state.Update(vars)
		snap := p.state.Get()
		logx.Debugf("UPS 状态: status=%q charge=%.0f%% in=%.1fV out=%.1fV runtime=%ds",
			snap.StatusRaw, snap.BatteryCharge, snap.InputVoltage, snap.OutputVoltage, snap.RuntimeSeconds)
		if p.OnUpdate != nil {
			p.OnUpdate()
		}
	}

	poll()
	for {
		select {
		case <-ctx.Done():
			logx.Infof("NUT 轮询已停止 (%s)", p.target.String())
			return
		case <-ticker.C:
			poll()
		}
	}
}
