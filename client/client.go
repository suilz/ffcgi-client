package client

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
)

// FastCgi Client的Golang实现
// 陈文宇

// -------------------1.参数设定-------------------

// 最大值定义
const (
	maxWrite = 65535 // maximum record body 单个消息的最大长度限制
	maxPad   = 255   // 最大填充长度
)

// 填充用数据
// for padding so we don't have to allocate all the time
// not synchronized because we don't care what the contents are
var pad [maxPad]byte

// -------------------2.Header-------------------

// recType 消息类型定义
// recType is a record type, as defined by
// https://web.archive.org/web/20150420080736/http://www.fastcgi.com/drupal/node/6?q=node/22#S8
type recType uint8

// 消息类型定义
const (
	typeBeginRequest    recType = iota + 1        // (Client) 表示一次请求的开始
	typeAbortRequest                              // (Client) 表示终止一次请求
	typeEndRequest                                // (Server) 表示一次请求结束
	typeParams                                    // (Client) 表示一个向FastCGI服务器传递的环境变量
	typeStdin                                     // (Client) 表示向FastCGI服务器传递的标准输入(请求数据)
	typeStdout                                    // (Server) 表示FastCGI服务器的标准输出(应答数据)
	typeStderr                                    // (Server) 表示FastCGI服务器的标准错误输出(错误数据)
	typeData                                      // (Client) 向FastCGI服务器传递的额外数据
	typeGetValues                                 // (Client) 向FastCGI服务器询问一些环境变量
	typeGetValuesResult                           // (Server) 询问环境变量的结果
	typeUnknownType                               // 未知类型，可能用作拓展
	typeMaxType         recType = typeUnknownType // 类型的最大值
)

// header 消息头结构定义
type header struct {
	Version       uint8   // 协议版本
	Type          recType // 请求类型
	ID            uint16  // 请求id
	ContentLength uint16  // 内容长度
	PaddingLength uint8   // 填充字符长度
	Reserved      uint8   // 保留字段
}

// init 初始化header
func (h *header) init(recType recType, reqID uint16, contentLength int) {
	h.Version = 1    // 目前版本都是1
	h.Type = recType // 指定类型
	h.ID = reqID     // 指定这次请求ID
	// 消息体长度
	h.ContentLength = uint16(contentLength)
	// 取反（补码+1）后 位与& 111 保留后三位，以使相加得1000结尾（也就是ContentLength+PaddingLength相加肯定为8的倍数）
	h.PaddingLength = uint8(-contentLength & 7)
}

// -------------------3.Body-------------------

// Role 指定FastCGI服务器担当的角色定义
type role uint16

const (
	roleResponder  role = iota + 1 // 响应器，目前只实现了响应器
	roleAuthorizer                 // 认证器
	roleFilter                     // 过滤器
)

// protocolStatus 的常量定义
const (
	statusRequestComplete = iota // 请求正常完成
	statusCantMultiplex          // FastCGI服务器不支持并发处理，请求已被拒绝
	statusOverloaded             // FastCGI服务器耗尽了资源或达到限制，请求已被拒绝
	statusUnknownRole            // FastCGI不支持指定的role，请求已被拒绝
)

// 消息体定义-展示用，暂时不需要定义结构体

// 消息体定义 - 发起请求
// type bodyBeginRequest struct {
// 	role     role
// 	flags    uint8
// 	reserved [5]uint8
// }

// 消息体定义 - 结束请求
// type bodyEndRequest struct {
// 	appStatus      uint32
// 	protocolStatus uint8
// 	reserved       [3]uint8
// }

// -------------------4.消息/record-------------------

// record 消息定义
type record struct {
	h   header                  // 消息头
	buf [maxWrite + maxPad]byte // 消息体，数据缓冲buf
}

// read 从io.Reader中获取消息到record.buf
func (rec *record) read(r io.Reader) (err error) {
	// 从io.Reader中获取header，binary.BigEndian只会读取指定参数的固定长度值，此处为8字节（header）
	if err = binary.Read(r, binary.BigEndian, &rec.h); err != nil {
		return err
	}
	// 检验版本
	if rec.h.Version != 1 {
		return errors.New("fcgi: invalid header version")
	}
	// 计算body的长度
	n := int(rec.h.ContentLength) + int(rec.h.PaddingLength)
	// 读取body内容并填充
	if _, err = io.ReadFull(r, rec.buf[:n]); err != nil {
		return err
	}
	return nil
}

// content 从buf中读取消息内容
func (rec *record) content() []byte {
	// 根据header定义的内容长度获取
	return rec.buf[:rec.h.ContentLength]
}

// -------------------5.连接/Conn-------------------

// newConn 发起一个Conn
func newConn(rwc io.ReadWriteCloser) *conn {
	return &conn{rwc: rwc}
}

// 定义conn类型
// conn sends records over rwc
type conn struct {
	// conn互斥锁
	mutex sync.Mutex
	// ReadWriteCloser
	rwc io.ReadWriteCloser

	// 消息体，设定Buffer，以避免混乱分配
	// to avoid allocations
	buf bytes.Buffer
	// 消息头
	h header
}

// Close 关闭连接
func (c *conn) Close() error {
	// 加锁
	c.mutex.Lock()
	defer c.mutex.Unlock()
	// 调用底层关闭函数
	return c.rwc.Close()
}

// writeRecord 发送一个包含 header 和 body 的消息
// writeRecord writes and sends a single record.
func (c *conn) writeRecord(recType recType, reqID uint16, b []byte) error {
	// 加锁
	c.mutex.Lock()
	defer c.mutex.Unlock()
	// 重置buffer
	c.buf.Reset()
	// 初始化生成header
	c.h.init(recType, reqID, len(b))
	// 将header写入buf
	if err := binary.Write(&c.buf, binary.BigEndian, c.h); err != nil {
		return err
	}
	// 将body写入buf
	if _, err := c.buf.Write(b); err != nil {
		return err
	}
	// 将填充数据写入buf
	if _, err := c.buf.Write(pad[:c.h.PaddingLength]); err != nil {
		return err
	}
	// 写入rwc（io.ReadWriteCloser）
	_, err := c.rwc.Write(c.buf.Bytes())
	return err
}

// writeBeginRequest 发送一个开始请求(自描述型记录)
func (c *conn) writeBeginRequest(reqID uint16, role role, flags uint8) error {
	// 构造header：截取前8位作为首byte,紧跟着是第2 byte，flags
	b := [8]byte{byte(role >> 8), byte(role), flags}
	// 发送开始请求
	return c.writeRecord(typeBeginRequest, reqID, b[:])
}

// writeEndRequest 发送一个结束请求(自描述型记录)
func (c *conn) writeEndRequest(reqID uint16, appStatus int, protocolStatus uint8) error {
	// 构造一个8字节的消息体：appStatus uint32 protocolStatus uint8 reserved [3]uint8
	b := make([]byte, 8)
	// 先放入appStatus
	binary.BigEndian.PutUint32(b, uint32(appStatus))
	// 再放入protocolStatus
	b[4] = protocolStatus
	// 发送结束请求
	return c.writeRecord(typeEndRequest, reqID, b)
}

// writeAbortRequest 发送一个异常结束请求(自描述型记录)
func (c *conn) writeAbortRequest(reqID uint16) error {
	// 发送异常结束请求
	return c.writeRecord(typeAbortRequest, reqID, nil)
}

// writePairs 发送键值对数据（typeParams，流数据型记录）
func (c *conn) writePairs(recType recType, reqID uint16, pairs map[string]string) error {
	// 创建一个bufwriter
	w := newWriter(c, recType, reqID)
	// 先构造一个最大8字节的空间
	b := make([]byte, 8)
	for k, v := range pairs {

		// nameLength uint32/uint8
		// 计算nameLength的长度并把长度值填充进slice中，返回此值所占字节大小
		n := encodeSize(b, uint32(len(k)))

		// valueLength uint32/uint8
		// 计算valueLength的长度并把长度值填充进slice中，返回此值所占字节大小
		n += encodeSize(b[n:], uint32(len(v)))
		// 截取有效的字节大小部分，将nameLength valueLength的信息写入buf
		if _, err := w.Write(b[:n]); err != nil {
			return err
		}
		// nameData 参数名
		// 将参数名（字符串）写入buf
		if _, err := w.WriteString(k); err != nil {
			return err
		}
		// valueData 对应的参数值
		// 将参数值（字符串）写入buf
		if _, err := w.WriteString(v); err != nil {
			return err
		}
	}
	// 发送并关闭bufwriter
	w.Close()
	return nil
}

// -------------------6.bufWriter-------------------

// newWriter 创建一个bufWriter
// 返回基于streamWriter的bufWriter
// 伪代码：bufWriter{ closer:streamWriter, bufio.Writer(streamWriter)}
func newWriter(c *conn, recType recType, reqID uint16) *bufWriter {
	// 创建 streamWriter
	s := &streamWriter{c: c, recType: recType, reqID: reqID}
	// 基于 streamWriter 创建 bufio.Writer（buf尺寸指定为最少maxWrite字节）
	w := bufio.NewWriterSize(s, maxWrite)
	return &bufWriter{s, w}
}

// bufWriter 包装了bufio.Writer，在关闭bufWriter时会关闭底层流
// bufWriter encapsulates bufio.Writer but also closes the underlying stream when Closed.
type bufWriter struct {
	closer io.Closer
	*bufio.Writer
}

// Close 关闭bufWriter，并关闭底层流
func (w *bufWriter) Close() error {
	// 关闭上层bufWriter前先尝试调用bufio.Writer的Flush方法
	// 将缓冲中的数据写入下层的io.Writer（streamwriter.Write）接口
	if err := w.Writer.Flush(); err != nil {
		// err，调用closer，关闭bufWriter
		w.closer.Close()
		return err
	}
	// 调用closer，关闭bufWriter
	return w.closer.Close()
}

// -------------------7.streamWriter-------------------

// streamWriter 处理流数据型记录的io.Writer，单次最多发送maxWrite bytes数据，bufWriter的底层实现
// streamWriter abstracts out the separation of a stream into discrete records.
// It only writes maxWrite bytes at a time.
type streamWriter struct {
	c       *conn   // 连接
	recType recType // 此次写入的消息类型
	reqID   uint16  // 请求ID
}

// Write 通过conn.writeRecord发送消息
// 实现 io.Writer 接口
// 返回写入的字节数
func (w *streamWriter) Write(p []byte) (int, error) {
	// 统计字节数
	nn := 0
	for len(p) > 0 {
		n := len(p)
		// 限制最大字节数
		if n > maxWrite {
			n = maxWrite
		}
		// 发送消息
		if err := w.c.writeRecord(w.recType, w.reqID, p[:n]); err != nil {
			return nn, err
		}
		nn += n
		// 截取
		p = p[n:]
	}
	return nn, nil
}

// Close 发送一个空消息，以告知server端此类型消息已经发送结束
// 实现 io.Closer 接口
func (w *streamWriter) Close() error {
	// send empty record to close the stream
	return w.c.writeRecord(w.recType, w.reqID, nil)
}

// -------------------8.其他函数-------------------

// readSize 返回参数名/值的长度值和自身所占的字节数
func readSize(s []byte) (uint32, int) {
	// 二进制内容为空，返回0, 0
	if len(s) == 0 {
		return 0, 0
	}
	// 获取第一个字节，以此判断是4字节还是1字节
	size, n := uint32(s[0]), 1
	// size（第一个字节）的最高位（标志位）为1时，表示4字节
	if size&(1<<7) != 0 {
		// 不足四字节，返回0, 0
		if len(s) < 4 {
			return 0, 0
		}
		n = 4
		// 转换为对应的长度值
		size = binary.BigEndian.Uint32(s)

		// &^= 双目运算符，将运算符左边数据相异的位保留，相同位清零
		// 1、如果右侧是0，则左侧数保持不变
		// 2、如果右侧是1，则左侧数一定清零
		// 此处作用是将size的最高位置为0
		size &^= 1 << 31
	}
	return size, n
}

// readString 从二进制内容中获取指定长度的字符串
func readString(s []byte, size uint32) string {
	// 如果二进制内容的长度不足，则返回空字串
	if size > uint32(len(s)) {
		return ""
	}
	// 从内容中截取指定长度，并转换为字符串返回
	return string(s[:size])
}

// encodeSize 计算键值对参数长度所占字节数并将长度值写入b
// 长度成员的第一个字节的最高位为标志位，为 0 则表示本长度编码为 1 字节，为 1 则表示编码为 4 字节
func encodeSize(b []byte, size uint32) int {
	// 如果长度大于127字节，则需要4字节来表示长度
	if size > 127 {
		// 长度的最高位置为1，其他不变
		size |= 1 << 31
		// 转换为uint32并写入b
		binary.BigEndian.PutUint32(b, size)
		// 返回所占字节数
		return 4
	}
	// 长度小于127字节，用1字节表示长度
	b[0] = byte(size)
	return 1
}

// -------------------9.调用方法-------------------

// Client Client define
type Client struct {
	conn *conn
}

// Close 关闭客户端
// Close implements Client.Close
// If the inner connection has been closed before,
// this method would do nothing and return nil
func (c *Client) Close() (err error) {
	if c.conn == nil {
		return
	}
	err = c.conn.Close()
	c.conn = nil
	return
}

// NewClient 新建一个Client
func NewClient(address string) (c *Client, err error) {
	// 定义一个网络连接
	netconn, err := net.Dial("tcp", address)
	// 包装为Client
	c = &Client{
		conn: &conn{
			rwc: netconn,
		},
	}
	return
}

// Request 请求方法
func (c *Client) Request(paramsMap map[string]string, reqStr string) (retout []byte, reterr []byte, err error) {

	// 指定请求ID
	var reqID uint16 = 1
	defer c.Close()

	// 不保持连接，keepalive逻辑还没有处理
	var keepalive uint8
	// 发起一个开始消息
	err = c.conn.writeBeginRequest(reqID, roleResponder, keepalive)
	if err != nil {
		return
	}
	// 发送键值对
	err = c.conn.writePairs(typeParams, reqID, paramsMap)
	if err != nil {
		return
	}
	// 是否需要发送请求数据
	if len(reqStr) > 0 {
		err = c.conn.writeRecord(typeStdin, reqID, []byte(reqStr))
		if err != nil {
			return
		}
	}

	// 处理接收的数据
	// 构造一个空消息
	rec := &record{}
	var err1 error

readLoop:
	// recive untill EOF or FCGI_END_REQUEST
	for {
		err1 = rec.read(c.conn.rwc)
		if err1 != nil {
			if err1 != io.EOF {
				err = err1
			}
			break
		}
		switch {
		case rec.h.Type == typeStdout:
			retout = append(retout, rec.content()...)
		case rec.h.Type == typeStderr:
			reterr = append(reterr, rec.content()...)
		case rec.h.Type == typeEndRequest:
			break readLoop
		default:
			break
		}
	}

	return
}
