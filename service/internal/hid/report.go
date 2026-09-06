package hid

import (
	"encoding/binary"
	"math"
	"strings"

	"UPS-Helper/internal/ups"
)

// 无效/未报告哨兵值。USB HID 报告字段数量在描述符里固定，无法真正"不传"，
// 故用超出正常范围的哨兵值表达"无此数据"，让飞牛的 usbhid-ups 识别为"暂无数据"
// 而非错误的 0。runtime 的 0xFFFF 被 NUT 官方文档明确认可为"剩余时间无限/未知"。
const (
	invalidU16 = 0xFFFF // 16 位字段无效值
	invalidU8  = 0xFF   // 8 位字段无效值
)

// BuildReport 按 Report ID 构造 HID 报告。首字节固定为 reportID。
//
// "有就传、没有就不传"原则：不同品牌 UPS 上报的 NUT 变量集合不同（如部分机型
// 无 battery.runtime / input.frequency / input.transfer.* / device.serial），
// 缺失的字段必须填哨兵值（0xFFFF/0xFF），绝不硬编码成某个品牌的值、也不填错误的 0。
func BuildReport(reportID byte, s ups.Snapshot) []byte {
	buf := []byte{reportID}
	switch reportID {
	case 0x1d:
		// Product String ID（iProduct）：返回 String Index 2，指向 USB 字符串
		// 描述符 index=2（iProduct）。真实型号由 d.Product()（device.model）返回。
		return append(buf, 0x02)

	case 0x1f:
		// Serial Number String ID：序列号存在返回 3（指向 USB 字符串描述符
		// index=3），缺失返回 0（无字符串索引）。须与设备描述符 iSerialNumber
		// 字段保持一致：缺序列号时主机不应查询 index 3，否则读到空串描述符会令
		// usbhid-ups 枚举困惑、反复重试。
		if s.Serial != "" {
			return append(buf, 0x03)
		}
		return append(buf, 0x00)

	case 0x03:
		// iManufacturer String ID：有厂商名返回 1，否则 0；iOEMInformation 无数据返回 0。
		mfr := byte(0x00)
		if s.Mfr != "" {
			mfr = 0x01
		}
		return append(buf, mfr, 0x00)

	case 0x04:
		// iDeviceChemistry String ID（无对应数据，返回 0 = 无字符串）。
		return append(buf, 0x00)

	case 0x06:
		// Rechargable=1, 电量单位=2(%)。设备固有属性，与品牌无关。
		return append(buf, 0x01, 0x02)

	case 0x07:
		// 容量相关：警告容量/剩余容量限制从 NUT 读（battery.charge.warning/low），
		// 其余（设计容量/容量粒度/满充容量）无标准 NUT 变量，保留通用默认。
		warn := uint8(20)
		if v, ok := s.FloatOK("battery.charge.warning"); ok {
			warn = uint8(clampPctInt(v))
		}
		low := uint8(10)
		if v, ok := s.FloatOK("battery.charge.low"); ok {
			low = uint8(clampPctInt(v))
		}
		return append(buf, 0x64, 0x05, 0x0A, warn, low, 0x64)

	case 0x3f:
		// 电池标称电压（×10）：读 battery.voltage.nominal，缺失填无效值。
		if v, ok := s.FloatOK("battery.voltage.nominal"); ok {
			return putU16(buf, scaledU16(v))
		}
		return putU16(buf, invalidU16)

	case 0x3e:
		// 标称功率：有功读 ups.realpower.nominal，视在读 ups.power.nominal，缺失填无效值。
		b := buf
		if v, ok := s.FloatOK("ups.realpower.nominal"); ok {
			b = putU16(b, uint16(math.Round(v)))
		} else {
			b = putU16(b, invalidU16)
		}
		if v, ok := s.FloatOK("ups.power.nominal"); ok {
			b = putU16(b, uint16(math.Round(v)))
		} else {
			b = putU16(b, invalidU16)
		}
		return b

	case 0x85:
		// Test Result = 6 (No test initiated)。固定语义。
		return append(buf, 0x06)

	case 0x88:
		// 输入额定电压（伏特，不缩放）：读 input.voltage.nominal，缺失填无效值。
		if v, ok := s.FloatOK("input.voltage.nominal"); ok {
			return putU16(buf, uint16(math.Round(v)))
		}
		return putU16(buf, invalidU16)

	case 0x83:
		// 低压转移点（伏特）：读 input.transfer.low，缺失填无效值。
		if v, ok := s.FloatOK("input.transfer.low"); ok {
			return putU16(buf, uint16(math.Round(v)))
		}
		return putU16(buf, invalidU16)

	case 0x84:
		// 高压转移点（伏特）：读 input.transfer.high，缺失填无效值。
		if v, ok := s.FloatOK("input.transfer.high"); ok {
			return putU16(buf, uint16(math.Round(v)))
		}
		return putU16(buf, invalidU16)

	case 0x86:
		// DelayBeforeShutdown（秒）：读 ups.delay.shutdown，缺失填 -1（未配置）。
		if v, ok := s.FloatOK("ups.delay.shutdown"); ok {
			return putU16(buf, uint16(math.Round(v)))
		}
		return putU16(buf, invalidU16)

	case 0x87:
		// DelayBeforeStartup（秒）：读 ups.delay.start，缺失填 -1（未配置）。
		if v, ok := s.FloatOK("ups.delay.start"); ok {
			return putU16(buf, uint16(math.Round(v)))
		}
		return putU16(buf, invalidU16)

	case 0x20:
		// 电量(0-100, 必填) + 电池电压(×10, 缺失填无效值)
		b := append(buf, byte(clampPctInt(s.BatteryCharge)))
		if s.Has("battery.voltage") {
			return putU16(b, scaledU16(s.BatteryVoltage))
		}
		return putU16(b, invalidU16)

	case 0x21:
		// 剩余供电时间：上报了 battery.runtime 即传真实值（无论在线/离线），
		// 让飞牛按其真实秒数换算分钟显示，并与直连真实 UPS 的 fnOS 页面一致。
		// 仅当该变量确实缺失时才填 0xFFFF（NUT 约定"无限/未知"）。
		// 注：fnOS 不认 0xFFFF 哨兵，会直接当 65535 秒算（→1092 分钟），故在线也必须发真值。
		if s.RuntimeSeconds > 0 && s.Has("battery.runtime") {
			return putU16(buf, uint16(s.RuntimeSeconds))
		}
		return putU16(buf, invalidU16)

	case 0x82:
		// 低电量报警时间（秒）：读 battery.runtime.low，缺失填无效值。
		if v, ok := s.FloatOK("battery.runtime.low"); ok {
			return putU16(buf, uint16(math.Round(v)))
		}
		return putU16(buf, invalidU16)

	case 0x22:
		// UPS 状态字：ACPresent|Charging|Discharging|FullyCharged|LowBattery
		var st byte
		if s.OnLine {
			st |= 0x01
		}
		if s.Charging {
			st |= 0x02
		}
		if s.Discharging {
			st |= 0x04
		}
		if s.OnLine && s.Charging {
			st |= 0x08 // FullyCharged（近似：在线且充电）
		}
		if s.LowBattery {
			st |= 0x20 // BelowRemainingCapacityLimit
		}
		return append(buf, st)

	case 0x28:
		// 稳压状态：Overload|Boost|Buck
		var st byte
		if s.Overload {
			st |= 0x01
		}
		return append(buf, st)

	case 0x80:
		// AudibleAlarmControl：读 ups.beeper.status（enabled/disabled/muted → 2/1/3），
		// 缺失或未知时默认 Disabled(1)。不再硬编码固定值。
		v := byte(0x01) // Disabled
		switch strings.ToLower(strings.TrimSpace(s.Vars["ups.beeper.status"])) {
		case "enabled":
			v = 0x02
		case "muted":
			v = 0x03
		}
		return append(buf, v)

	case 0x23:
		// 输入/输出电压(×10)，缺失填无效值
		b := buf
		if s.Has("input.voltage") {
			b = putU16(b, scaledU16(s.InputVoltage))
		} else {
			b = putU16(b, invalidU16)
		}
		if s.Has("output.voltage") {
			b = putU16(b, scaledU16(s.OutputVoltage))
		} else {
			b = putU16(b, invalidU16)
		}
		return b

	case 0x2a:
		// 输入频率(×10)，缺失填无效值
		if s.Has("input.frequency") {
			return putU16(buf, uint16(math.Round(s.InputFrequency*10)))
		}
		return putU16(buf, invalidU16)

	// 源 UPS 不回报有效负载，上报会令 fnOS 恒显示 0%，故不纳入上报字段。

	default:
		// 未知报告 ID：返回空。
		return nil
	}
}

// putU16 追加一个 16 位小端值到 buf。
func putU16(buf []byte, v uint16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, v)
	return append(buf, b...)
}

// scaledU16 把物理量按 ×10 缩放并四舍五入为 uint16（电压/频率约定 ×10）。
func scaledU16(f float64) uint16 {
	return uint16(math.Round(f * 10))
}

func clampPctInt(f float64) int {
	v := int(math.Round(f))
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
