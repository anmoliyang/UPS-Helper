// Package usbdev 实现被模拟的 USB HID UPS 设备：描述符、控制传输与中断报告。
package usbdev

import (
	"encoding/binary"
	"sync"
	"unicode/utf16"

	"UPS-Helper/internal/hid"
	"UPS-Helper/internal/logx"
	"UPS-Helper/internal/ups"
	"UPS-Helper/internal/usbip"
)

// 模拟设备标识：CyberPower 厂商 0764，产品 ID 0501，Full-speed，总线 ID 1-1。
//
// VID:PID 必须保持 0764:0501：飞牛的 usbhid-ups 按精确 VID:PID 白名单识别 CyberPower
// UPS，未登记的 PID 会被判为 "unknown product" 而不被当作 UPS，故不得改用其它值。
//
// 厂商名/序列号/型号等显示名一律从 NUT 动态读取（见 Device 字段的 Set* 方法），绝不硬编码。
const (
	VendorID  = 0x0764 // CyberPower Systems（厂商必须保持，否则不被识别为 UPS）
	ProductID = 0x0501 // 必须用飞牛白名单内的真实 CyberPower PID，否则不识别为 UPS
	BusID     = "1-1"
	BusNum    = 1
	DevNum    = 1

	SysfsPath = "/sys/devices/platform/ups_helper/usb1/1-1"

	// DefaultProduct 是 NUT 尚未上报真实型号时的占位产品名；型号就绪后立即被
	// device.model 覆盖，正常运行时不会被用户看到。
	DefaultProduct = "UPS Remote"
)

// USB 描述符类型。
const (
	descDevice        = 0x01
	descConfiguration = 0x02
	descString        = 0x03
	descInterface     = 0x04
	descEndpoint      = 0x05
	descDeviceQual    = 0x06
	descHID           = 0x21
	descHIDReport     = 0x22
)

// 标准与类请求。
const (
	reqGetStatus        = 0x00
	reqClearFeature     = 0x01
	reqSetFeature       = 0x03
	reqSetAddress       = 0x05
	reqGetDescriptor    = 0x06
	reqGetConfiguration = 0x08
	reqSetConfiguration = 0x09
	reqGetInterface     = 0x0A
	reqSetInterface     = 0x0B

	hidReqGetReport   = 0x01
	hidReqGetIdle     = 0x02
	hidReqGetProtocol = 0x03
	hidReqSetReport   = 0x09
	hidReqSetIdle     = 0x0A
	hidReqSetProtocol = 0x0B
)

// Device 是模拟的 USB HID UPS 设备，实现 usbip.Device 接口。
type Device struct {
	state *ups.State

	// 身份字段由多个 goroutine 读写（NUT 轮询的 notifyReport、配置热重载的
	// ApplyConfig、挂载前置的 startAutoMount），须加锁避免数据竞态。
	mu sync.RWMutex

	// product 是 iProduct 字符串（USB 枚举时主机读到），默认 DefaultProduct，
	// 可通过 SetProduct 覆盖为真实 UPS 型号（device.model）。
	product string
	// manufacturer 是 iManufacturer 字符串，由 NUT 的 device.mfr 动态设置。
	manufacturer string
	// serial 是 iSerialNumber 字符串，由 NUT 的 device.serial 动态设置。
	serial string
}

// New 创建模拟设备，状态来源为 state。
// 厂商名/序列号/型号均为运行时从 NUT 动态填充（SetManufacturer/SetSerial/SetProduct），
// 初始为空，避免任何硬编码显示名。
func New(state *ups.State) *Device {
	return &Device{state: state, product: DefaultProduct}
}

// SetProduct 设置产品名（iProduct 字符串）。传入空串时回退到默认名。
func (d *Device) SetProduct(p string) {
	if p == "" {
		p = DefaultProduct
	}
	d.mu.Lock()
	d.product = p
	d.mu.Unlock()
}

// SetManufacturer 设置厂商名（iManufacturer 字符串），来源 NUT 的 device.mfr。
func (d *Device) SetManufacturer(m string) {
	d.mu.Lock()
	d.manufacturer = m
	d.mu.Unlock()
}

// SetSerial 设置序列号（iSerialNumber 字符串），来源 NUT 的 device.serial。
func (d *Device) SetSerial(s string) {
	d.mu.Lock()
	d.serial = s
	d.mu.Unlock()
}

// Product 返回当前产品名（iProduct 字符串）。
func (d *Device) Product() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.product
}

// Manufacturer 返回当前厂商名。
func (d *Device) Manufacturer() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.manufacturer
}

// Serial 返回当前序列号（iSerialNumber 字符串）。
// 序列号缺失时返回空串：设备描述符 iSerialNumber 字段同步置 0（见 deviceDescriptor），
// 主机不会去查询字符串描述符 index 3，避免读到空串描述符令 usbhid-ups 枚举困惑、
// 反复重试。部分 UPS（如 CyberPower UT650EGC）本就无序列号，属正常。
func (d *Device) Serial() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.serial
}

// Info 返回 USB/IP 设备描述（1 个 HID 接口）。
func (d *Device) Info() usbip.DeviceInfo {
	return usbip.DeviceInfo{
		Path:               SysfsPath,
		BusID:              BusID,
		BusNum:             BusNum,
		DevNum:             DevNum,
		Speed:              usbip.SpeedFull,
		IDVendor:           VendorID,
		IDProduct:          ProductID,
		BCDDevice:          0x0001,
		DeviceClass:        0x00,
		DeviceSubClass:     0x00,
		DeviceProtocol:     0x00,
		ConfigurationValue: 1,
		NumConfigurations:  1,
		NumInterfaces:      1,
		Interfaces: []usbip.InterfaceInfo{
			{Class: 0x03, SubClass: 0x00, Protocol: 0x00}, // HID，无引导接口
		},
	}
}

// Report 按 Report ID 构造 HID 报告（用于控制端点 GET_REPORT）。
func (d *Device) Report(reportID byte) []byte {
	return hid.BuildReport(reportID, d.state.Get())
}

// Control 处理 EP0 控制传输。
func (d *Device) Control(setup usbip.SetupPacket, out []byte) ([]byte, int32) {
	reqType := setup.RequestType & 0x60 // 0x00 标准, 0x20 类, 0x40 厂商
	recipient := setup.RequestType & 0x1F

	logx.Debugf("usb: setup bmRequestType=0x%02x bRequest=0x%02x wValue=0x%04x wIndex=0x%04x wLength=%d",
		setup.RequestType, setup.Request, setup.Value, setup.Index, setup.Length)

	switch reqType {
	case 0x00:
		return d.standardRequest(setup, recipient)
	case 0x20:
		return d.classRequest(setup, out)
	default:
		logx.Debugf("usb: 不支持的请求类型 0x%02x，返回 STALL", setup.RequestType)
		return nil, usbip.StatusEPIPE
	}
}

func (d *Device) standardRequest(setup usbip.SetupPacket, recipient uint8) ([]byte, int32) {
	switch setup.Request {
	case reqGetDescriptor:
		descType := uint8(setup.Value >> 8)
		descIndex := uint8(setup.Value & 0xFF)
		return d.getDescriptor(descType, descIndex, setup.Index)

	case reqSetConfiguration:
		logx.Debugf("usb: SET_CONFIGURATION %d", setup.Value&0xFF)
		return nil, usbip.StatusOK

	case reqGetConfiguration:
		return []byte{0x01}, usbip.StatusOK

	case reqGetStatus:
		return []byte{0x00, 0x00}, usbip.StatusOK

	case reqSetInterface:
		return nil, usbip.StatusOK

	case reqGetInterface:
		return []byte{0x00}, usbip.StatusOK

	case reqClearFeature, reqSetFeature, reqSetAddress:
		return nil, usbip.StatusOK

	default:
		logx.Debugf("usb: 未实现的标准请求 0x%02x (recipient=0x%02x)，返回 STALL", setup.Request, recipient)
		return nil, usbip.StatusEPIPE
	}
}

func (d *Device) getDescriptor(descType, descIndex uint8, langOrIface uint16) ([]byte, int32) {
	switch descType {
	case descDevice:
		return d.deviceDescriptor(), usbip.StatusOK

	case descConfiguration:
		return configDescriptor(), usbip.StatusOK

	case descString:
		return d.stringDescriptor(descIndex)

	case descHID:
		return hidDescriptor(), usbip.StatusOK

	case descHIDReport:
		logx.Debugf("usb: 主机请求 HID 报告描述符 (%d 字节)", len(hid.ReportDescriptor))
		return hid.ReportDescriptor, usbip.StatusOK

	case descDeviceQual:
		// Full-speed 设备无 device qualifier，按规范 STALL
		return nil, usbip.StatusEPIPE

	default:
		logx.Debugf("usb: 未知描述符类型 0x%02x，返回 STALL", descType)
		return nil, usbip.StatusEPIPE
	}
}

func (d *Device) classRequest(setup usbip.SetupPacket, out []byte) ([]byte, int32) {
	switch setup.Request {
	case hidReqGetReport:
		reportType := uint8(setup.Value >> 8) // 1=Input, 2=Output, 3=Feature
		reportID := uint8(setup.Value & 0xFF)
		if reportType == 0x01 || reportType == 0x03 {
			report := d.Report(reportID)
			logx.Debugf("usb: HID GET_REPORT type=%d id=0x%02x -> %d bytes [% x]", reportType, reportID, len(report), report)
			return report, usbip.StatusOK
		}
		// Output 报告(0x02)或其它：返回空。Output 是主机下发的控制命令，
		// 虚拟 UPS 不执行，按规范以空负载回应。
		logx.Debugf("usb: HID GET_REPORT type=%d 不支持，返回空", reportType)
		return nil, usbip.StatusOK

	case hidReqSetReport:
		logx.Debugf("usb: HID SET_REPORT 已忽略 (%d 字节)", len(out))
		return nil, usbip.StatusOK

	case hidReqGetIdle:
		return []byte{0x00}, usbip.StatusOK

	case hidReqSetIdle:
		logx.Debugf("usb: HID SET_IDLE duration=%d", setup.Value>>8)
		return nil, usbip.StatusOK

	case hidReqGetProtocol:
		return []byte{0x01}, usbip.StatusOK

	case hidReqSetProtocol:
		return nil, usbip.StatusOK

	default:
		logx.Debugf("usb: 未实现的 HID 类请求 0x%02x，返回 STALL", setup.Request)
		return nil, usbip.StatusEPIPE
	}
}

// ---------------------------------------------------------------------------
// 描述符构造（USB 描述符内多字节字段为小端）
// ---------------------------------------------------------------------------

// deviceDescriptor 返回 18 字节设备描述符。
// iSerialNumber 字段动态决定：NUT 上报了真实序列号（device.serial）时才指向
// 字符串描述符 index 3，否则写 0（表示设备无序列号）。若序列号为空仍声明 index 3，
// 主机会去读到一个空字符串描述符，导致 usbhid-ups 枚举不稳定（表现为"开关几次才识别"）。
func (d *Device) deviceDescriptor() []byte {
	b := make([]byte, 18)
	b[0] = 18                                     // bLength
	b[1] = descDevice                             // bDescriptorType
	binary.LittleEndian.PutUint16(b[2:4], 0x0110) // bcdUSB 1.10
	b[4] = 0x00                                   // bDeviceClass（由接口决定）
	b[5] = 0x00                                   // bDeviceSubClass
	b[6] = 0x00                                   // bDeviceProtocol
	b[7] = 0x08                                   // bMaxPacketSize0
	binary.LittleEndian.PutUint16(b[8:10], VendorID)
	binary.LittleEndian.PutUint16(b[10:12], ProductID)
	binary.LittleEndian.PutUint16(b[12:14], 0x0001) // bcdDevice
	b[14] = 0x01                                    // iManufacturer
	b[15] = 0x02                                    // iProduct
	if d.Serial() != "" {                           // iSerialNumber：有序列号才指向 index 3
		b[16] = 0x03
	} else {
		b[16] = 0x00
	}
	b[17] = 0x01 // bNumConfigurations
	return b
}

// configDescriptor 返回配置描述符集合：Config(9) + Interface(9) + HID(9) + Endpoint(7) = 34。
func configDescriptor() []byte {
	iface := interfaceDescriptor()
	hidDesc := hidDescriptor()
	ep := endpointDescriptor()
	total := 9 + len(iface) + len(hidDesc) + len(ep)

	cfg := make([]byte, 9)
	cfg[0] = 9
	cfg[1] = descConfiguration
	binary.LittleEndian.PutUint16(cfg[2:4], uint16(total)) // wTotalLength
	cfg[4] = 0x01                                          // bNumInterfaces
	cfg[5] = 0x01                                          // bConfigurationValue
	cfg[6] = 0x00                                          // iConfiguration
	cfg[7] = 0x80                                          // bmAttributes: 总线供电
	cfg[8] = 25                                            // bMaxPower: 50mA

	out := make([]byte, 0, total)
	out = append(out, cfg...)
	out = append(out, iface...)
	out = append(out, hidDesc...)
	out = append(out, ep...)
	return out
}

// interfaceDescriptor 返回 9 字节接口描述符（HID 类）。
func interfaceDescriptor() []byte {
	return []byte{
		9,             // bLength
		descInterface, // bDescriptorType
		0x00,          // bInterfaceNumber
		0x00,          // bAlternateSetting
		0x01,          // bNumEndpoints
		0x03,          // bInterfaceClass: HID
		0x00,          // bInterfaceSubClass: 无引导接口
		0x00,          // bInterfaceProtocol
		0x00,          // iInterface
	}
}

// hidDescriptor 返回 9 字节 HID 描述符。
func hidDescriptor() []byte {
	b := make([]byte, 9)
	b[0] = 9
	b[1] = descHID
	binary.LittleEndian.PutUint16(b[2:4], 0x0110) // bcdHID 1.10
	b[4] = 0x00                                   // bCountryCode
	b[5] = 0x01                                   // bNumDescriptors
	b[6] = descHIDReport                          // bDescriptorType
	binary.LittleEndian.PutUint16(b[7:9], uint16(len(hid.ReportDescriptor)))
	return b
}

// endpointDescriptor 返回 7 字节端点描述符（EP1 IN，中断）。
func endpointDescriptor() []byte {
	b := make([]byte, 7)
	b[0] = 7
	b[1] = descEndpoint
	b[2] = 0x81                                                        // bEndpointAddress: EP1 IN
	b[3] = 0x03                                                        // bmAttributes: 中断传输
	binary.LittleEndian.PutUint16(b[4:6], uint16(hid.InputReportSize)) // wMaxPacketSize
	b[6] = 20                                                          // bInterval: 20ms
	return b
}

// stringDescriptor 返回字符串描述符（UTF-16LE）。厂商/型号/序列号均来自
// 设备动态字段（由 NUT 的 device.mfr/device.model/device.serial 设置），不硬编码。
// 序列号字段无值时返回 STALL（此时设备描述符 iSerialNumber 已置 0，主机不应查询）。
func (d *Device) stringDescriptor(index uint8) ([]byte, int32) {
	switch index {
	case 0:
		// 语言 ID 列表：仅 en-US (0x0409)
		return []byte{0x04, descString, 0x09, 0x04}, usbip.StatusOK
	case 1:
		return encodeString(d.Manufacturer()), usbip.StatusOK
	case 2:
		return encodeString(d.Product()), usbip.StatusOK
	case 3:
		if s := d.Serial(); s != "" {
			return encodeString(s), usbip.StatusOK
		}
		return nil, usbip.StatusEPIPE
	default:
		// 未定义的字符串索引返回空字符串（bLength=2，合法）而非 STALL：
		// usbhid-ups 会按设备描述符与 HID 报告里出现的索引逐个探测字符串，
		// 对未实现的索引 STALL 会让 libusb 返回 EINVAL，日志里表现为反复刷
		// "nut_libusb_get_string: Invalid parameter"，驱动不断重试、状态抖动，
		// 飞牛的 UPS 页面因此要反复刷新才显示内容。返回空串可安全满足这些探测。
		return []byte{0x02, descString}, usbip.StatusOK
	}
}

func encodeString(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2, 2+len(units)*2)
	out[0] = byte(2 + len(units)*2)
	out[1] = descString
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}
