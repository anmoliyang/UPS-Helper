package ups

import (
	"strconv"
	"strings"
)

// getFloat 读取 NUT 变量并转为 float64，缺失或非法时返回 0。
func getFloat(vars map[string]string, key string) float64 {
	raw, ok := vars[key]
	if !ok {
		return 0
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	return v
}

// clampPct 将百分比限制在 0-100。
func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// Has 报告指定 NUT 变量是否真实存在且非空。用于实现"有就传、没有就不传"：
// 不同品牌 UPS 上报的变量集合不同（如部分机型无 battery.runtime / input.frequency /
// device.serial），缺失的字段不能当成"值为 0"，而应视为"无数据"。
func (s Snapshot) Has(key string) bool {
	raw, ok := s.Vars[key]
	return ok && strings.TrimSpace(raw) != ""
}

// FloatOK 读取指定 NUT 变量，返回 (数值, 是否存在)。缺失、空串或非法时 ok=false，
// 与 Has 一致地区分"字段不存在"与"字段值为 0"。
func (s Snapshot) FloatOK(key string) (float64, bool) {
	raw, ok := s.Vars[key]
	if !ok {
		return 0, false
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
