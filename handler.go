package ffcgiclient

import (
	"bytes"
	"log"
	"net/http"
)

// Handler 实现http.Handler并提供记录logger方法
type Handler interface {
	http.Handler
	SetLogger(logger *log.Logger)
}

// NewHandler 返回默认的Http.Handler实现
func NewHandler(requestHandler RequestHandler, clientFactory ClientFactory) Handler {
	return &defaultHandler{
		requestHandler: requestHandler, // 请求处理Handler
		newClient:      clientFactory,  // client
	}
}

// defaultHandler Http.Handler的实现
type defaultHandler struct {
	requestHandler RequestHandler // 请求Handler
	newClient      ClientFactory  // client工厂方法
	logger         *log.Logger    // 日志
}

// SetLogger 设置日志
func (h *defaultHandler) SetLogger(logger *log.Logger) {
	h.logger = logger
}

// ServeHTTP 主处理逻辑，实现http.Handler接口
func (h *defaultHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	// 创建fcgi client
	// 测试
	// fmt.Println("【ServeHTTP】初始化")
	c, err := h.newClient()
	if err != nil {
		// 返回502
		http.Error(w, "failed to connect to FastCGI application", http.StatusBadGateway)
		log.Printf("unable to connect to FastCGI application. %s",
			err.Error())
		return
	}

	// TODO 测试keepalive连接的保持/关闭情况
	// 延迟关闭
	defer func() {
		if c == nil {
			return
		}
		// 关闭client
		if err = c.Close(); err != nil {
			log.Printf("error closing client: %s",
				err.Error())
		}
	}()

	// 处理请求
	// 测试
	// fmt.Println("【ServeHTTP】开始处理请求")
	resp, err := h.requestHandler(c, NewRequest(r))
	// 测试
	// fmt.Println("【ServeHTTP】处理请求完成")
	if err != nil {
		// 返回500
		http.Error(w, "failed to process request", http.StatusInternalServerError)
		log.Printf("unable to process request %s",
			err.Error())
		return
	}
	// Buffer
	errBuffer := new(bytes.Buffer)
	// 测试
	// fmt.Println("【ServeHTTP】准备开始WriteTo")
	err = resp.WriteTo(w, errBuffer)
	// 测试
	// fmt.Println("【ServeHTTP】完成WriteTo")
	if err != nil {
		// 返回500
		http.Error(w, "failed to write stream", http.StatusInternalServerError)
		log.Printf("Unable WriteTo: %s",
			err.Error())
		return
	}

	if errBuffer.Len() > 0 {
		log.Printf("error stream from application process %s",
			errBuffer.String())
	}
}
