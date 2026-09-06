package hid

import (
	"fmt"
	"testing"

	"UPS-Helper/internal/ups"
)

// TestBuildReportPresent 验证"有数据就传真实值"：模拟一台完整上报所有可选字段的 UPS。
func TestBuildReportPresent(t *testing.T) {
	st := ups.NewState()
	st.Update(map[string]string{
		"battery.charge":          "80",
		"battery.voltage":         "13.5",
		"input.voltage":           "230",
		"output.voltage":          "230",
		"ups.load":                "42",
		"battery.runtime":         "1800",
		"input.frequency":         "500", // 0.1Hz
		"input.voltage.nominal":   "220",
		"input.transfer.low":      "140",
		"input.transfer.high":     "295",
		"battery.runtime.low":     "300",
		"ups.realpower.nominal":   "360",
		"ups.power.nominal":       "400",
		"battery.voltage.nominal": "12",
		"battery.charge.warning":  "20",
		"battery.charge.low":      "10",
		"device.mfr":              "CPS",
		"device.serial":           "SN123",
		"device.model":            "UT650EGC",
		"ups.status":              "OL CHRG",
		"ups.beeper.status":       "enabled",
		"ups.delay.shutdown":      "20",
		"ups.delay.start":         "30",
	})
	s := st.Get()

	cases := []struct {
		id   byte
		want []byte
	}{
		{0x1d, []byte{0x1d, 0x02}},
		{0x1f, []byte{0x1f, 0x03}},           // 有序列号
		{0x03, []byte{0x03, 0x01, 0x00}},     // 有厂商名，无 OEM 信息
		{0x20, []byte{0x20, 80, 0x87, 0x00}}, // 电量 80 + 电压 13.5*10=135=0x87
		// 在线时也发送真实剩余时间（1800s=0x708）。
		// fnOS 不识别 0xFFFF 哨兵，会直接按 65535 秒折算成约 1092 分钟显示在 UPS 页，
		// 故需发送真实剩余时间而非哨兵值。
		{0x21, []byte{0x21, 0x08, 0x07}},
		{0x22, []byte{0x22, 0x0b}},                   // OL|CHRG|FullyCharged = 0x0b
		{0x23, []byte{0x23, 0xfc, 0x08, 0xfc, 0x08}}, // 2300=0x8FC
		{0x2a, []byte{0x2a, 0xf4, 0x01}},             // 频率 500=0x1F4
		{0x88, []byte{0x88, 0xdc, 0x00}},             // 额定 220V=0xDC
		{0x83, []byte{0x83, 0x8c, 0x00}},             // 低压转移 140V=0x8C
		{0x84, []byte{0x84, 0x27, 0x01}},             // 高压转移 295V=0x127
		{0x82, []byte{0x82, 0x2c, 0x01}},             // 低电时间 300s=0x12C
		{0x3e, []byte{0x3e, 0x68, 0x01, 0x90, 0x01}}, // 360W=0x168, 400VA=0x190
		{0x3f, []byte{0x3f, 0x78, 0x00}},             // 电池标称 12V*10=120=0x78
		{0x80, []byte{0x80, 0x02}},                   // 蜂鸣器 enabled=2
		{0x86, []byte{0x86, 0x14, 0x00}},             // 关机延时 20s=0x14
		{0x87, []byte{0x87, 0x1e, 0x00}},             // 开机延时 30s=0x1E
	}
	for _, c := range cases {
		got := BuildReport(c.id, s)
		if fmt.Sprintf("% x", got) != fmt.Sprintf("% x", c.want) {
			t.Errorf("report 0x%02x = [% x], want [% x]", c.id, got, c.want)
		} else {
			fmt.Printf("OK 0x%02x = [% x]\n", c.id, got)
		}
	}

	// 离线（放电）时同样传真实值（1800s = 0x708）。
	stOB := ups.NewState()
	stOB.Update(map[string]string{
		"battery.charge":  "50",
		"battery.runtime": "1800",
		"ups.status":      "OB DISCHRG",
	})
	got := BuildReport(0x21, stOB.Get())
	if want := []byte{0x21, 0x08, 0x07}; fmt.Sprintf("% x", got) != fmt.Sprintf("% x", want) {
		t.Errorf("离线 report 0x21 = [% x], want [% x]", got, want)
	} else {
		fmt.Printf("OK 离线 0x21 = [% x]\n", got)
	}

	// 仅当 UPS 确实不上报 battery.runtime 时，才回落 0xFFFF 哨兵。
	stNoRT := ups.NewState()
	stNoRT.Update(map[string]string{
		"battery.charge": "80",
		"ups.status":     "OL",
	})
	got = BuildReport(0x21, stNoRT.Get())
	if want := []byte{0x21, 0xff, 0xff}; fmt.Sprintf("% x", got) != fmt.Sprintf("% x", want) {
		t.Errorf("缺失 battery.runtime 时 report 0x21 = [% x], want [% x]", got, want)
	} else {
		fmt.Printf("OK 缺失 battery.runtime 回落哨兵 0x21 = [% x]\n", got)
	}
}

// TestBuildReportMissing 验证"没有就不传"：模拟仅上报核心字段、缺失大量可选字段的 UPS。
func TestBuildReportMissing(t *testing.T) {
	st := ups.NewState()
	st.Update(map[string]string{
		"battery.charge": "100",
		"ups.status":     "OL",
	})
	s := st.Get()

	cases := []struct {
		id   byte
		want []byte
	}{
		{0x1f, []byte{0x1f, 0x00}},                   // 无序列号
		{0x03, []byte{0x03, 0x00, 0x00}},             // 无厂商名
		{0x20, []byte{0x20, 100, 0xff, 0xff}},        // 有电量，无电池电压
		{0x21, []byte{0x21, 0xff, 0xff}},             // 无剩余时间
		{0x23, []byte{0x23, 0xff, 0xff, 0xff, 0xff}}, // 无输入/输出电压
		{0x2a, []byte{0x2a, 0xff, 0xff}},             // 无频率
		{0x88, []byte{0x88, 0xff, 0xff}},             // 无额定电压
		{0x83, []byte{0x83, 0xff, 0xff}},             // 无低压转移
		{0x84, []byte{0x84, 0xff, 0xff}},             // 无高压转移
		{0x82, []byte{0x82, 0xff, 0xff}},             // 无低电时间
		{0x3e, []byte{0x3e, 0xff, 0xff, 0xff, 0xff}}, // 无额定功率
		{0x3f, []byte{0x3f, 0xff, 0xff}},             // 无电池标称电压
		{0x80, []byte{0x80, 0x01}},                   // 无蜂鸣器状态 → 默认 disabled=1
		{0x86, []byte{0x86, 0xff, 0xff}},             // 无关机延时 → -1
		{0x87, []byte{0x87, 0xff, 0xff}},             // 无开机延时 → -1
	}
	for _, c := range cases {
		got := BuildReport(c.id, s)
		if fmt.Sprintf("% x", got) != fmt.Sprintf("% x", c.want) {
			t.Errorf("缺失场景 report 0x%02x = [% x], want [% x]", c.id, got, c.want)
		} else {
			fmt.Printf("OK 缺失 0x%02x = [% x]\n", c.id, got)
		}
	}
	fmt.Printf("descriptor length = %d bytes\n", len(ReportDescriptor))
}
