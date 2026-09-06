// Package ups 维护从 NUT 拉取的 UPS 状态缓存，供 HID 报告与 HTTP API 读取。
package ups

import (
	"math"
	"strings"
	"sync"
)

// Snapshot 是某一时刻的 UPS 状态快照（对外只读）。
type Snapshot struct {
	// 原始 NUT 变量（键 -> 值）
	Vars map[string]string `json:"vars"`

	InputVoltage   float64 `json:"input_voltage"`
	OutputVoltage  float64 `json:"output_voltage"`
	BatteryVoltage float64 `json:"battery_voltage"`
	BatteryCharge  float64 `json:"battery_charge"`
	RuntimeSeconds int     `json:"battery_runtime"`
	InputFrequency float64 `json:"input_frequency"`
	// Model 是 NUT 上报的真实 UPS 型号（device.model，如 UT650EGC），
	// 直接作为 iProduct，无需 SSH 或手动填写。
	Model string `json:"model"`
	// Mfr 是 NUT 上报的真实厂商名（device.mfr，如 CPS），用于 USB 字符串描述符
	// 的 iManufacturer，避免硬编码厂商名。
	Mfr string `json:"mfr"`
	// Serial 是 NUT 上报的序列号（device.serial），用于 iSerialNumber；部分 UPS 未
	// 提供时为回空字符串。
	Serial    string `json:"serial"`
	StatusRaw string `json:"ups_status"`

	// 状态标志（从 ups.status 解析）
	OnLine      bool `json:"online"`      // OL
	OnBattery   bool `json:"on_battery"`  // OB
	LowBattery  bool `json:"low_battery"` // LB
	Overload    bool `json:"overload"`    // OVER
	Charging    bool `json:"charging"`    // CHRG
	Discharging bool `json:"discharging"` // DISCHRG

	// 元信息
	Connected bool   `json:"connected"`
	LastError string `json:"last_error"`
}

// State 是并发安全的 UPS 状态缓存。
type State struct {
	mu   sync.RWMutex
	snap Snapshot
}

// NewState 创建一个初始未连接的状态缓存。
func NewState() *State {
	return &State{snap: Snapshot{Vars: map[string]string{}, StatusRaw: "OFF"}}
}

// Update 用一组 NUT 变量刷新缓存。
func (s *State) Update(vars map[string]string) {
	snap := Snapshot{
		Vars:      vars,
		Connected: true,
	}
	snap.InputVoltage = getFloat(vars, "input.voltage")
	snap.OutputVoltage = getFloat(vars, "output.voltage")
	snap.BatteryVoltage = getFloat(vars, "battery.voltage")
	snap.BatteryCharge = clampPct(getFloat(vars, "battery.charge"))
	snap.RuntimeSeconds = int(getFloat(vars, "battery.runtime"))
	// NUT 的 input.frequency 单位是 0.1Hz（如 500 表示 50.0Hz），此处 ÷10 取整，
	// 再交由 HID 报告层 ×10 还原为飞牛可读的 0.1Hz 值。
	snap.InputFrequency = math.Round(getFloat(vars, "input.frequency") / 10)
	snap.Model = strings.TrimSpace(vars["device.model"])
	snap.Mfr = strings.TrimSpace(vars["device.mfr"])
	snap.Serial = strings.TrimSpace(vars["device.serial"])
	snap.StatusRaw = strings.TrimSpace(vars["ups.status"])
	snap.applyStatusFlags()

	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
}

// SetDisconnected 标记为断连并记录错误信息（保留最后一次数值）。
func (s *State) SetDisconnected(errMsg string) {
	s.mu.Lock()
	s.snap.Connected = false
	s.snap.LastError = errMsg
	s.mu.Unlock()
}

// Get 返回当前快照的拷贝。
func (s *State) Get() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.snap
	out.Vars = make(map[string]string, len(s.snap.Vars))
	for k, v := range s.snap.Vars {
		out.Vars[k] = v
	}
	return out
}

func (snap *Snapshot) applyStatusFlags() {
	fields := strings.Fields(strings.ToUpper(snap.StatusRaw))
	for _, f := range fields {
		switch f {
		case "OL":
			snap.OnLine = true
		case "OB":
			snap.OnBattery = true
		case "LB":
			snap.LowBattery = true
		case "OVER":
			snap.Overload = true
		case "CHRG":
			snap.Charging = true
		case "DISCHRG":
			snap.Discharging = true
		}
	}
	// 兜底：既未上报 OL 也未上报 OB 时，默认按在线处理，
	// 避免 ACPresent 恒为 0 令主机误判为断电。
	if !snap.OnLine && !snap.OnBattery {
		snap.OnLine = true
	}
}
