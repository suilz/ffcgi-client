package ffcgiclient

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// client部分

// NewRequest 返回一个标准FastCgi请求
func NewRequest(r *http.Request) (req *Request) {
	req = &Request{
		Raw:          r,                       // 保留原始请求
		Role:         roleResponder,           // 目前Role只支持roleResponder
		Params:       make(map[string]string), // 键值对参数
		FlagKeepConn: 1,                       // keepAlive
	}

	// 在客户端，如果Body是nil表示该请求没有主体写入GET请求
	if r == nil {
		return
	}

	// 将Body写入标准输入stdin
	req.Stdin = r.Body
	return
}

// Request 包含FastCGI信息的标准请求
type Request struct {
	Raw          *http.Request     // http请求元数据
	Role         role              // 指定FastCGI服务器担当的角色定义
	Params       map[string]string // 键值对参数
	Stdin        io.ReadCloser     // 标准输入数据
	Data         io.ReadCloser     // 额外数据
	FlagKeepConn uint8             // 完成后是否保持连接
}

// idPool 请求id生成池
type idPool struct {
	IDs chan uint16
}

// Alloc 从ID池中分配一个ID
func (p *idPool) Alloc() uint16 {
	return <-p.IDs
}

// ReleaseID 释放使用的ID
func (p *idPool) Release(id uint16) {
	go func() {
		// 释放ID回ID池，重用ID
		// 使用goroutine，避免阻塞
		// TODO 改为缓冲池？
		p.IDs <- id
		// 测试流程
		// fmt.Println("【Release】释放ID回ID池:" + string(id))
	}()
}

// newIDPool 创建一个请求ID生成池
func newIDPool(limit uint32) (p idPool) {

	// 限制ID数量
	if limit == 0 || limit > 65535 {
		limit = 65535
	}

	// 创建一个chan，用于存放请求ID
	idsChan := make(chan uint16)
	go func(maxID uint16) {
		for i := uint16(1); i <= maxID; i++ {
			idsChan <- i
		}
	}(uint16(limit))

	p.IDs = idsChan
	return
}

// client 是Client接口的实现
type client struct {
	conn        *conn       // 请求连接
	connFactory ConnFactory // 创建新连接工厂方法
	idPool      idPool      // 请求ID池
}

// writeRequest client发起一个包含params和stdin的fastcgi请求
func (c *client) writeRequest(reqID uint16, req *Request) (err error) {

	// 发生错误时发起一个异常结束消息
	defer func() {
		if err != nil {
			c.conn.writeAbortRequest(reqID)
			return
		}
	}()

	// 发起一个开始消息
	err = c.conn.writeBeginRequest(reqID, req.Role, req.FlagKeepConn)
	if err != nil {
		return
	}
	// 发送键值对参数
	err = c.conn.writePairs(typeParams, reqID, req.Params)
	if err != nil {
		return
	}

	// 发送标准输入
	if req.Stdin != nil {
		// 创建一个标准输入bufWriter
		stdinWriter := newWriter(c.conn, typeStdin, reqID)
		// 延后关闭stdin
		defer req.Stdin.Close()

		// 每次获取最多1024字节数据
		p := make([]byte, 1024)
		var count int
		for {
			// 从标准输入中获取数据
			count, err = req.Stdin.Read(p)
			if err == io.EOF {
				err = nil
			} else if err != nil {
				stdinWriter.Close()
				return
			}
			if count == 0 {
				break
			}
			// 将获取到的部分写入buf
			_, err = stdinWriter.Write(p[:count])
			if err != nil {
				stdinWriter.Close()
				return
			}
		}
		// 发送并关闭bufwriter
		if err = stdinWriter.Close(); err != nil {
			return
		}
	}

	return
}

// readResponse 读取fastcgi的stdout和stderr信息，写入ResponsePipe
func (c *client) readResponse(ctx context.Context, resp *ResponsePipe, req *Request) (err error) {
	// 构造一个空消息
	var rec record
	done := make(chan int)

	// 开启新的协程循环读取处理
	go func() {
	readLoop:
		for {
			// 测试
			// fmt.Println("【readResponse】读取fastcgi的stdout和stderr信息，写入ResponsePipe，读取消息")
			// 读取消息
			if err := rec.read(c.conn.rwc); err != nil {
				// 测试
				// fmt.Println("read 错误：" + err.Error())
				// if err == io.EOF {
				// 	continue
				// }
				break
			}
			// 不同输出类型获取不同的流
			switch rec.h.Type {
			case typeStdout:
				// 写入stdOutWriter
				resp.stdOutWriter.Write(rec.content())
			case typeStderr:
				// 写入stdErrWriter
				resp.stdErrWriter.Write(rec.content())
			case typeEndRequest:
				// 结束中断循环
				break readLoop
			default:
				// 异常，返回自定义错误
				err := fmt.Sprintf("unexpected type %#v in readLoop", rec.h.Type)
				resp.stdErrWriter.Write([]byte(err))
			}
		}
		// 测试
		// fmt.Println("【readResponse】读取fastcgi的stdout和stderr信息，写入ResponsePipe，处理完成")
		// 处理完成发起关闭信号
		close(done)
	}()

	select {
	case <-ctx.Done():
		// 上下文取消
		err = fmt.Errorf("timeout or canceled")
	case <-done:
		// 处理完毕
	}
	return
}

// Do 实现Client.Do方法，是业务主逻辑
func (c *client) Do(req *Request) (resp *ResponsePipe, err error) {

	// 分配请求ID
	reqID := c.idPool.Alloc()

	// 测试
	// fmt.Println("【Client.Do】创建responsePipe")
	// 创建responsePipe
	resp = NewResponsePipe()
	// 创建Err通道和完成信号通道
	rwError, allDone := make(chan error), make(chan int)

	// 检查连接
	if c.conn == nil {
		err = fmt.Errorf("client connection has been closed")
		return
	}

	// 如果是原始请求，则使用其附带的上下文
	var ctx context.Context
	if req.Raw != nil {
		ctx = req.Raw.Context()
	} else {
		ctx = context.TODO()
	}

	// 定义WaitGroup，等待所有读写完成
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		wg.Wait()
		// 测试
		// fmt.Println("【Client.Do】读写完成")
		close(allDone)
	}()

	// 并行执行读写
	// 写入请求
	go func() {
		// 测试
		// fmt.Println("【Client.Do】写入请求开始")
		if err := c.writeRequest(reqID, req); err != nil {
			rwError <- err
		}
		// 测试
		// fmt.Println("【Client.Do】写入请求完成")
		wg.Done()
	}()

	// 读，从client获取响应并通过responsePipe写入响应
	go func() {

		// 测试
		// fmt.Println("【Client.Do】读取请求开始")
		if err := c.readResponse(ctx, resp, req); err != nil {
			rwError <- err
		}
		// 测试
		// fmt.Println("【Client.Do】读取请求并通过responsePipe写入响应")
		wg.Done()
	}()

	// 不要阻止client.Do返回并返回响应管道，否则会被没有使用的响应管道阻塞
	go func() {
		// 等待处理完成或超时
	loop:
		for {
			select {
			case err := <-rwError:
				// 将获取到的Err写入buf
				resp.stdErrWriter.Write([]byte(err.Error()))
				continue
			case <-allDone:
				// 处理完成，跳出循环
				break loop
			}
		}

		// 测试
		// fmt.Println("【Client.Do】处理完成，释放资源")
		// 关闭/释放资源
		c.idPool.Release(reqID)
		resp.Close()
		close(rwError)
	}()
	return
}

// Close Client.Close的实现
func (c *client) Close() (err error) {
	return c.CloseConn()
}

// CloseConn 如果之前已关闭内部连接，则此方法将不执行任何操作并返回nil
func (c *client) CloseConn() (err error) {
	// 测试
	// fmt.Println("【Client.Close】关闭连接")
	if c.conn == nil {
		return
	}
	// 关闭连接
	err = c.conn.Close()
	c.conn = nil
	// 测试
	// fmt.Println("【Client.Close】conn置空")
	return
}

// NewConn 使用conn工厂为client创建一个连接
func (c *client) NewConn() (err error) {
	// 测试
	// fmt.Println("【Client.NewConn】创建conn")
	conn, err := c.connFactory()
	if err != nil {
		return
	}
	c.conn = newConn(conn)
	return
}

// Client 是FastCGI的客户端接口定义
// 应用程序进程通过给定的连接进行通信（net.Conn）
type Client interface {

	// 执行FastCGI请求
	// 返回响应流（stdout和stderr）和错误
	// 注意：协议错误将写入ResponsePipe中的stderr流
	Do(req *Request) (resp *ResponsePipe, err error)

	NewConn() error

	CloseConn() error

	// 关闭底层连接
	Close() error
}

// ConnFactory 新创建与fastcgiServer通信的网络连接
type ConnFactory func() (net.Conn, error)

// SimpleConnFactory 创建最简单的ConnFactory实现
func SimpleConnFactory(network, address string) ConnFactory {
	return func() (net.Conn, error) {
		return net.Dial(network, address)
	}
}

// ClientFactory client工厂，创建新的包含conn的fastcgi客户端
type ClientFactory func() (Client, error)

// SimpleClientFactory 返回根据传入的ConnFactory而实现的client工厂方法
// limit 是fastcgi server所支持的最大请求数，0即代表最大值65535，默认:0
func SimpleClientFactory(connFactory ConnFactory, limit uint32) ClientFactory {
	return func() (c Client, err error) {
		// 连接指定的地址
		conn, err := connFactory()
		if err != nil {
			return
		}

		// 创建client
		c = &client{
			conn:        newConn(conn),    // 连接
			connFactory: connFactory,      // 工厂方法
			idPool:      newIDPool(limit), // 请求ID池
		}
		return
	}
}

// SimpleClientFactoryNoConn 返回根据传入的ConnFactory而实现的client工厂方法
// limit 是fastcgi server所支持的最大请求数，0即代表最大值65535，默认:0
// 此方法不预先创建连接
func SimpleClientFactoryNoConn(connFactory ConnFactory, limit uint32) ClientFactory {
	return func() (c Client, err error) {
		// 创建client
		c = &client{
			conn:        nil,              // 连接
			connFactory: connFactory,      // 工厂方法
			idPool:      newIDPool(limit), // 请求ID池
		}
		return
	}
}

// NewResponsePipe 返回一个初始化的ResponsePipe
func NewResponsePipe() (p *ResponsePipe) {
	p = new(ResponsePipe)
	// 创建同步的内存中的管道Pipe
	p.stdOutReader, p.stdOutWriter = io.Pipe()
	p.stdErrReader, p.stdErrWriter = io.Pipe()
	return
}

// ResponsePipe 结构体定义，响应Response的管道结构，主要用作Request返回的响应的中间介质
// 包含可以处理FastCGI输出流的readers和writers
type ResponsePipe struct {
	stdOutReader io.Reader
	stdOutWriter io.WriteCloser
	stdErrReader io.Reader
	stdErrWriter io.WriteCloser
}

// Close 关闭所有的writer
func (pipes *ResponsePipe) Close() {
	pipes.stdOutWriter.Close()
	pipes.stdErrWriter.Close()
}

// WriteTo 将给定的输出/错误写入http.ResponseWriter/io.Writer
func (pipes *ResponsePipe) WriteTo(rw http.ResponseWriter, ew io.Writer) (err error) {
	chErr := make(chan error, 2)
	defer close(chErr)

	var wg sync.WaitGroup
	wg.Add(2)

	// 开启协程处理响应输出
	go func() {
		// 测试
		// fmt.Println("【WriteTo】将给定的输出写入http.ResponseWriter/io.Writer，写入开始")
		chErr <- pipes.writeResponse(rw)
		// 测试
		// fmt.Println("【WriteTo】将给定的输出写入http.ResponseWriter/io.Writer，写入完成")
		wg.Done()
	}()
	// 开启协程处理错误输出
	go func() {
		// 测试
		// fmt.Println("【WriteTo】将给定的错误写入http.ResponseWriter/io.Writer，写入开始")
		chErr <- pipes.writeError(ew)
		// 测试
		// fmt.Println("【WriteTo】将给定的错误写入http.ResponseWriter/io.Writer，写入完成")
		wg.Done()
	}()

	// 等待处理完毕
	wg.Wait()
	for i := 0; i < 2; i++ {
		if err = <-chErr; err != nil {
			return
		}
	}
	return
}

// writeError 将给定的错误写入io.Writer
func (pipes *ResponsePipe) writeError(w io.Writer) (err error) {
	_, err = io.Copy(w, pipes.stdErrReader)
	if err != nil {
		err = fmt.Errorf("copy error: %v", err.Error())
	}
	return
}

// writeResponse 将给定的输出写入http.ResponseWriter
func (pipes *ResponsePipe) writeResponse(w http.ResponseWriter) (err error) {
	// 测试
	// fmt.Println("【writeResponse】将给定的输出写入http.ResponseWriter：初始化")
	// 创建一个具有最少有size尺寸的缓冲、从stdOutReader读取的*Reader
	linebody := bufio.NewReaderSize(pipes.stdOutReader, 1024)
	// 初始化http.Header，该值会被WriteHeader方法发送
	headers := make(http.Header)
	// 状态码
	statusCode := 0
	// 记录header行数
	headerLines := 0
	// 标记是否空行
	sawBlankLine := false

	// 循环处理Header
	for {
		var line []byte
		var isPrefix bool
		// 读取一行
		line, isPrefix, err = linebody.ReadLine()
		// 如果行太长超过了缓冲，返回值isPrefix会被设为true
		if isPrefix {
			// header值过长，发送500
			w.WriteHeader(http.StatusInternalServerError)
			err = fmt.Errorf("long header line from subprocess")
			return
		}
		// 遇到结束符，跳出循环
		if err == io.EOF {
			break
		}
		// 错误
		if err != nil {
			// 发送500
			w.WriteHeader(http.StatusInternalServerError)
			err = fmt.Errorf("error reading headers: %v", err)
			return
		}
		// 空行结束，跳出循环
		if len(line) == 0 {
			sawBlankLine = true
			break
		}
		// header行数+1
		headerLines++
		// 以:切割字符串，获取此行的header参数
		parts := strings.SplitN(string(line), ":", 2)
		// 少于2个元素，返回错误
		if len(parts) < 2 {
			err = fmt.Errorf("bogus header line: %s", string(line))
			return
		}
		// 赋值
		headerName, headerVal := parts[0], parts[1]
		// 将前后端所有空白（unicode.IsSpace指定）都去掉
		headerName = strings.TrimSpace(headerName)
		headerVal = strings.TrimSpace(headerVal)

		switch {
		case headerName == "Status":
			// 处理状态码
			// 状态码格式是3位，少于3则返回错误
			if len(headerVal) < 3 {
				err = fmt.Errorf("bogus status (short): %q", headerVal)
				return
			}
			var code int
			code, err = strconv.Atoi(headerVal[0:3])
			if err != nil {
				err = fmt.Errorf("bogus status: %q\nline was %q",
					headerVal, line)
				return
			}
			statusCode = code
		default:
			// 除status外，其他header参数添加到headers
			headers.Add(headerName, headerVal)
		}
	}
	// 如果header行数为0或没有空行结束
	if headerLines == 0 || !sawBlankLine {
		// 测试
		// fmt.Println("【writeResponse】将给定的输出写入http.ResponseWriter：no headers写入错误")
		// 500
		w.WriteHeader(http.StatusInternalServerError)
		err = fmt.Errorf("no headers")
		return
	}

	// 获取Location值
	if loc := headers.Get("Location"); loc != "" {
		/*
			if strings.HasPrefix(loc, "/") && h.PathLocationHandler != nil {
				h.handleInternalRedirect(rw, req, loc)
				return
			}
		*/
		// 没有指定状态码，则置为302
		if statusCode == 0 {
			statusCode = http.StatusFound
		}
	}

	// 没有指定状态码，且Content-Type没有内容，返回500
	if statusCode == 0 && headers.Get("Content-Type") == "" {
		w.WriteHeader(http.StatusInternalServerError)
		err = fmt.Errorf("missing required Content-Type in headers")
		return
	}

	// 没有指定状态码，置为200
	if statusCode == 0 {
		statusCode = http.StatusOK
	}

	// 将headers复制到rw的Header
	for k, vv := range headers {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	// 写入并发送Header
	w.WriteHeader(statusCode)
	// 将剩下的数据拷贝并发送
	_, err = io.Copy(w, linebody)
	// fmt.Println(string(linebody.buf))
	if err != nil {
		err = fmt.Errorf("copy error: %v", err)
	}
	return
}

// ClientFunc 是Client接口的快捷函数实现，主要用于测试和开发
type ClientFunc func(req *Request) (resp *ResponsePipe, err error)

// Do implements Client.Do
func (c ClientFunc) Do(req *Request) (resp *ResponsePipe, err error) {
	return c(req)
}

// Close implements Client.Close
func (c ClientFunc) Close() error {
	return nil
}
