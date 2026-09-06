// Package usbip 实现 USB/IP 协议服务端。
//
// USB/IP 协议头（op_common、usbip_header_basic、cmd_submit、ret_submit、
// cmd_unlink、ret_unlink、usbip_usb_device）中所有多字节字段一律使用**网络字节序
// （大端）**。唯一例外是 cmd_submit 里内嵌的 8 字节 USB setup packet，它是原样
// 透传的裸 USB 数据，按 USB 规范为小端。
package usbip

import (
	"encoding/binary"
	"fmt"
	"io"
)

// 协议版本与操作码。
const (
	// Version 是 USB/IP 协议版本 1.1.1
	Version uint16 = 0x0111

	OpReqDevlist uint16 = 0x8005 // 客户端请求设备列表
	OpRepDevlist uint16 = 0x0005 // 服务端回复设备列表
	OpReqImport  uint16 = 0x8003 // 客户端请求导入（attach）设备
	OpRepImport  uint16 = 0x0003 // 服务端回复导入结果
)

// URB 阶段命令码。
const (
	CmdSubmit uint32 = 0x00000001 // USBIP_CMD_SUBMIT
	CmdUnlink uint32 = 0x00000002 // USBIP_CMD_UNLINK
	RetSubmit uint32 = 0x00000003 // USBIP_RET_SUBMIT
	RetUnlink uint32 = 0x00000004 // USBIP_RET_UNLINK
)

// 传输方向。
const (
	DirOut uint32 = 0 // USBIP_DIR_OUT
	DirIn  uint32 = 1 // USBIP_DIR_IN
)

// Linux errno（以负值返回给内核）。
const (
	StatusOK    int32 = 0
	StatusEPIPE int32 = -32 // -EPIPE，端点 STALL
)

// 结构体固定长度。
const (
	opHeaderLen    = 8   // op_common
	busIDLen       = 32  // usbip_usb_device.busid
	devPathLen     = 256 // usbip_usb_device.path
	usbDeviceLen   = 312 // sizeof(struct usbip_usb_device)
	usbIfaceLen    = 4   // sizeof(struct usbip_usb_interface)
	urbHeaderLen   = 48  // sizeof(struct usbip_header)
	isoPacketLen   = 16  // sizeof(struct usbip_iso_packet_descriptor)
	maxISOPackets  = 1024
	maxTransferLen = 1 << 20 // 1MiB 上限，防止恶意长度
)

// USB 速度枚举（drivers/usb/usbip 使用 enum usb_device_speed）。
const (
	SpeedLow  uint32 = 1
	SpeedFull uint32 = 2
	SpeedHigh uint32 = 3
)

// ---------------------------------------------------------------------------
// op_common
// ---------------------------------------------------------------------------

// OpHeader 对应 struct op_common。
type OpHeader struct {
	Version uint16
	Code    uint16
	Status  uint32
}

// ReadOpHeader 从 r 读取 8 字节操作头（大端）。
func ReadOpHeader(r io.Reader) (OpHeader, error) {
	var buf [opHeaderLen]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return OpHeader{}, err
	}
	return OpHeader{
		Version: binary.BigEndian.Uint16(buf[0:2]),
		Code:    binary.BigEndian.Uint16(buf[2:4]),
		Status:  binary.BigEndian.Uint32(buf[4:8]),
	}, nil
}

// AppendOpHeader 以大端追加 8 字节操作头。
func AppendOpHeader(dst []byte, code uint16, status uint32) []byte {
	var buf [opHeaderLen]byte
	binary.BigEndian.PutUint16(buf[0:2], Version)
	binary.BigEndian.PutUint16(buf[2:4], code)
	binary.BigEndian.PutUint32(buf[4:8], status)
	return append(dst, buf[:]...)
}

// ---------------------------------------------------------------------------
// usbip_usb_device / usbip_usb_interface
// ---------------------------------------------------------------------------

// InterfaceInfo 对应 struct usbip_usb_interface。
type InterfaceInfo struct {
	Class    uint8
	SubClass uint8
	Protocol uint8
}

// DeviceInfo 对应 struct usbip_usb_device。
type DeviceInfo struct {
	Path               string
	BusID              string
	BusNum             uint32
	DevNum             uint32
	Speed              uint32
	IDVendor           uint16
	IDProduct          uint16
	BCDDevice          uint16
	DeviceClass        uint8
	DeviceSubClass     uint8
	DeviceProtocol     uint8
	ConfigurationValue uint8
	NumConfigurations  uint8
	NumInterfaces      uint8
	Interfaces         []InterfaceInfo
}

// DevID 返回 USB/IP 的 devid：(busnum << 16) | devnum。
func (d DeviceInfo) DevID() uint32 {
	return (d.BusNum << 16) | d.DevNum
}

// AppendTo 以大端序列化为 312 字节。
func (d DeviceInfo) AppendTo(dst []byte) []byte {
	buf := make([]byte, usbDeviceLen)
	copyFixed(buf[0:devPathLen], d.Path)
	copyFixed(buf[256:256+busIDLen], d.BusID)
	binary.BigEndian.PutUint32(buf[288:292], d.BusNum)
	binary.BigEndian.PutUint32(buf[292:296], d.DevNum)
	binary.BigEndian.PutUint32(buf[296:300], d.Speed)
	binary.BigEndian.PutUint16(buf[300:302], d.IDVendor)
	binary.BigEndian.PutUint16(buf[302:304], d.IDProduct)
	binary.BigEndian.PutUint16(buf[304:306], d.BCDDevice)
	buf[306] = d.DeviceClass
	buf[307] = d.DeviceSubClass
	buf[308] = d.DeviceProtocol
	buf[309] = d.ConfigurationValue
	buf[310] = d.NumConfigurations
	buf[311] = d.NumInterfaces
	return append(dst, buf...)
}

// AppendInterfacesTo 追加所有接口描述（每个 4 字节）。
func (d DeviceInfo) AppendInterfacesTo(dst []byte) []byte {
	for _, iface := range d.Interfaces {
		dst = append(dst, iface.Class, iface.SubClass, iface.Protocol, 0x00)
	}
	return dst
}

// copyFixed 把字符串写入定长零填充缓冲区（超长则截断并保留结尾 NUL）。
func copyFixed(dst []byte, s string) {
	b := []byte(s)
	if len(b) > len(dst)-1 {
		b = b[:len(dst)-1]
	}
	copy(dst, b)
}

// ReadBusID 读取 32 字节 busid 并去掉零填充。
func ReadBusID(r io.Reader) (string, error) {
	var buf [busIDLen]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return "", err
	}
	end := 0
	for end < len(buf) && buf[end] != 0 {
		end++
	}
	return string(buf[:end]), nil
}

// ---------------------------------------------------------------------------
// URB 阶段报文
// ---------------------------------------------------------------------------

// URBHeader 对应 struct usbip_header（48 字节），联合体部分按命令解释。
type URBHeader struct {
	// usbip_header_basic
	Command   uint32
	Seqnum    uint32
	DevID     uint32
	Direction uint32
	Endpoint  uint32

	// cmd_submit 专用
	TransferFlags        uint32
	TransferBufferLength int32
	StartFrame           int32
	NumberOfPackets      int32
	Interval             int32
	Setup                [8]byte // 裸 USB setup packet：小端

	// cmd_unlink 专用
	UnlinkSeqnum uint32
}

// ReadURBHeader 读取并解析 48 字节 URB 头（大端）。
func ReadURBHeader(r io.Reader) (URBHeader, error) {
	var buf [urbHeaderLen]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return URBHeader{}, err
	}
	h := URBHeader{
		Command:   binary.BigEndian.Uint32(buf[0:4]),
		Seqnum:    binary.BigEndian.Uint32(buf[4:8]),
		DevID:     binary.BigEndian.Uint32(buf[8:12]),
		Direction: binary.BigEndian.Uint32(buf[12:16]),
		Endpoint:  binary.BigEndian.Uint32(buf[16:20]),
	}
	switch h.Command {
	case CmdSubmit:
		h.TransferFlags = binary.BigEndian.Uint32(buf[20:24])
		h.TransferBufferLength = int32(binary.BigEndian.Uint32(buf[24:28]))
		h.StartFrame = int32(binary.BigEndian.Uint32(buf[28:32]))
		h.NumberOfPackets = int32(binary.BigEndian.Uint32(buf[32:36]))
		h.Interval = int32(binary.BigEndian.Uint32(buf[36:40]))
		copy(h.Setup[:], buf[40:48])
	case CmdUnlink:
		h.UnlinkSeqnum = binary.BigEndian.Uint32(buf[20:24])
	}
	return h, nil
}

// BuildRetSubmit 构造 USBIP_RET_SUBMIT 报文（48 字节头 + 可选数据）。
//
// 按 Linux 实现约定，返回包的 devid/direction/ep 均置 0，内核仅按 seqnum 匹配 URB。
func BuildRetSubmit(seqnum uint32, status int32, data []byte) []byte {
	out := make([]byte, urbHeaderLen, urbHeaderLen+len(data))
	binary.BigEndian.PutUint32(out[0:4], RetSubmit)
	binary.BigEndian.PutUint32(out[4:8], seqnum)
	binary.BigEndian.PutUint32(out[8:12], 0)  // devid
	binary.BigEndian.PutUint32(out[12:16], 0) // direction
	binary.BigEndian.PutUint32(out[16:20], 0) // ep
	binary.BigEndian.PutUint32(out[20:24], uint32(status))
	binary.BigEndian.PutUint32(out[24:28], uint32(int32(len(data)))) // actual_length
	binary.BigEndian.PutUint32(out[28:32], 0)                        // start_frame
	binary.BigEndian.PutUint32(out[32:36], 0)                        // number_of_packets
	binary.BigEndian.PutUint32(out[36:40], 0)                        // error_count
	// out[40:48] padding 保持为 0
	return append(out, data...)
}

// BuildRetUnlink 构造 USBIP_RET_UNLINK 报文（48 字节）。
func BuildRetUnlink(seqnum uint32, status int32) []byte {
	out := make([]byte, urbHeaderLen)
	binary.BigEndian.PutUint32(out[0:4], RetUnlink)
	binary.BigEndian.PutUint32(out[4:8], seqnum)
	binary.BigEndian.PutUint32(out[8:12], 0)
	binary.BigEndian.PutUint32(out[12:16], 0)
	binary.BigEndian.PutUint32(out[16:20], 0)
	binary.BigEndian.PutUint32(out[20:24], uint32(status))
	// out[24:48] padding 保持为 0
	return out
}

// BuildDevlistReply 构造 OP_REP_DEVLIST 报文。
func BuildDevlistReply(devs []DeviceInfo) []byte {
	out := AppendOpHeader(nil, OpRepDevlist, 0)
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(devs)))
	out = append(out, n[:]...)
	for _, d := range devs {
		out = d.AppendTo(out)
		out = d.AppendInterfacesTo(out)
	}
	return out
}

// BuildImportReply 构造 OP_REP_IMPORT 报文。status != 0 时不携带设备信息。
func BuildImportReply(dev DeviceInfo, status uint32) []byte {
	out := AppendOpHeader(nil, OpRepImport, status)
	if status != 0 {
		return out
	}
	// OP_REP_IMPORT 只包含设备信息，不包含接口列表。
	return dev.AppendTo(out)
}

// ValidateTransferLength 校验 cmd_submit 声明的长度是否合理。
func ValidateTransferLength(n int32) error {
	if n < 0 || n > maxTransferLen {
		return fmt.Errorf("非法的 transfer_buffer_length: %d", n)
	}
	return nil
}
