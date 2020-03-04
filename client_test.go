package ffcgiclient

import (
	"log"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestClient(t *testing.T) {
	// 获取fastcgi应用程序服务器tcp地址
	address := os.Getenv("FASTCGI_ADDR")
	if address == "" {
		// address = "192.168.100.7:9000"
		address = "192.168.100.7:9001"
		// address = "127.0.0.1:9010"
	}
	yourPHPDocRoot := "/home/vagrant/code/php_test"
	// 根据地址生成conn工厂
	connFactory := SimpleConnFactory("tcp", address)

	// 静态资源处理
	http.Handle("/assets/",
		// StripPrefix返回一个处理器handler
		// 该处理器会将请求的URL.Path字段中给定前缀prefix去除后再交由handler处理
		// 没有给定前缀的请求回复404 page not found
		http.StripPrefix("/assets/",
			// FileServer返回一个使用FileSystem接口root提供文件访问服务的HTTP处理器
			// 要使用操作系统的FileSystem接口实现，可使用http.Dir：
			// http.Handle("/", http.FileServer(http.Dir("/tmp")))
			http.FileServer(http.Dir("/Users/seven/Works/golang/assets"))))

	// Pool
	pool := NewClientPool(
		SimpleClientFactoryNoConn(connFactory, 0),
		10,             // 通道缓冲数量，即预创建client的数量
		30*time.Second, // client存活时间
	)
	// 连接池模式
	http.Handle("/pool/", NewHandler(
		NewPHPFS(yourPHPDocRoot)(BasicHandler),
		pool.CreateClient,
	))

	// 普通模式
	http.Handle("/normal/", NewHandler(
		NewPHPFS(yourPHPDocRoot)(BasicHandler),
		SimpleClientFactory(connFactory, 0),
	))

	// serve at 8080 port
	log.Fatal(http.ListenAndServe(":8080", nil))

	http.ListenAndServe(":8080", nil)
}

func TestHttpHandle(t *testing.T) {
	// http.Handle("/", gofast.NewHandler(
	// 	gofast.NewPHPFS("/var/www/html")(gofast.BasicSession),
	// 	gofast.SimpleClientFactory(connFactory, 0),
	// ))
}
