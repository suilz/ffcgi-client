package ffcgiclient

import (
	"net"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// 处理请求流程的路由/参数映射/逻辑补充等

// RequestHandler 使用提供的client处理*Reqeust，正确处理路由和其他参数映射等
type RequestHandler func(client Client, req *Request) (resp *ResponsePipe, err error)

// Middleware 中间件将RequestHandler转换为另一个RequestHandler
// 该库提供的中间件有助于根据不同应用的需要映射fastcgi参数
// 您也可以实现自己的中间件，在其间添加额外的业务逻辑，从*ResponsePipe重写响应流或更好地处理错误等
// 以下为Nginx中常见的 fastcgi 参数:
//
// fastcgi_param  SCRIPT_FILENAME    $document_root$fastcgi_script_name;
// fastcgi_param  PATH_INFO          $fastcgi_path_info;
// fastcgi_param  PATH_TRANSLATED    $document_root$fastcgi_path_info;
// fastcgi_param  QUERY_STRING       $query_string;
// fastcgi_param  REQUEST_METHOD     $request_method;
// fastcgi_param  CONTENT_TYPE       $content_type;
// fastcgi_param  CONTENT_LENGTH     $content_length;
// fastcgi_param  SCRIPT_NAME        $fastcgi_script_name;
// fastcgi_param  REQUEST_URI        $request_uri;
// fastcgi_param  DOCUMENT_URI       $document_uri;
// fastcgi_param  DOCUMENT_ROOT      $document_root;
// fastcgi_param  SERVER_PROTOCOL    $server_protocol;
// fastcgi_param  HTTPS              $https if_not_empty;
// fastcgi_param  GATEWAY_INTERFACE  CGI/1.1;
// fastcgi_param  SERVER_SOFTWARE    nginx/$nginx_version;
// fastcgi_param  REMOTE_ADDR        $remote_addr;
// fastcgi_param  REMOTE_PORT        $remote_port;
// fastcgi_param  SERVER_ADDR        $server_addr;
// fastcgi_param  SERVER_PORT        $server_port;
// fastcgi_param  SERVER_NAME        $server_name;
// # PHP only, required if PHP was built with --enable-force-cgi-redirect
// fastcgi_param  REDIRECT_STATUS    200;
type Middleware func(RequestHandler) RequestHandler

// Chain 将多个中间件链接成一个中间件
// 中间件将RequestHandler循环链式传递调用，RequestHandler3(RequestHandler2(RequestHandler1()))...
// 第一个中间件将处理客户端和请求，最后一个则处理ResponsePipe和错误
func Chain(middlewares ...Middleware) Middleware {
	if len(middlewares) == 0 {
		return nil
	}
	return func(inner RequestHandler) (out RequestHandler) {
		out = inner
		for i := len(middlewares) - 1; i >= 0; i-- {
			out = middlewares[i](out)
		}
		return
	}
}

// BasicHandler 默认的基础handler
func BasicHandler(client Client, req *Request) (*ResponsePipe, error) {
	return client.Do(req)
}

// BasicParamsMapMiddleware [中间件]基础参数映射中间件（只包含基本的http协议参数），它将基本参数映射到req.Params
// Parameters included:
// CONTENT_TYPE
// CONTENT_LENGTH
// HTTPS
// GATEWAY_INTERFACE
// REMOTE_ADDR
// REMOTE_PORT
// SERVER_PORT
// SERVER_NAME
// SERVER_PROTOCOL
// SERVER_SOFTWARE
// REDIRECT_STATUS
// REQUEST_METHOD
// REQUEST_SCHEME
// REQUEST_URI
// QUERY_STRING
func BasicParamsMapMiddleware(inner RequestHandler) RequestHandler {
	return func(client Client, req *Request) (*ResponsePipe, error) {
		// 获取原始请求
		r := req.Raw
		// 根据原始请求的TLS判断是否Https（https在SSL/TLS层上加密传输）
		isHTTPS := r.TLS != nil
		if isHTTPS {
			req.Params["HTTPS"] = "on"
		}
		// 解析请求地址
		remoteAddr, remotePort, _ := net.SplitHostPort(r.RemoteAddr)
		// 解析server地址
		host, serverPort, err := net.SplitHostPort(r.Host)
		if err != nil {
			if isHTTPS {
				serverPort = "443"
			} else {
				serverPort = "80"
			}
		}

		// 填充基础信息
		req.Params["CONTENT_TYPE"] = r.Header.Get("Content-Type")
		req.Params["CONTENT_LENGTH"] = r.Header.Get("Content-Length")
		req.Params["GATEWAY_INTERFACE"] = "CGI/1.1"
		req.Params["REMOTE_ADDR"] = remoteAddr
		req.Params["REMOTE_PORT"] = remotePort
		req.Params["SERVER_PORT"] = serverPort
		req.Params["SERVER_NAME"] = host
		req.Params["SERVER_PROTOCOL"] = r.Proto
		req.Params["SERVER_SOFTWARE"] = "GolangFastcgi"
		req.Params["REDIRECT_STATUS"] = "200"
		req.Params["REQUEST_SCHEME"] = r.URL.Scheme
		req.Params["REQUEST_METHOD"] = r.Method
		req.Params["REQUEST_URI"] = r.RequestURI
		req.Params["QUERY_STRING"] = r.URL.RawQuery

		return inner(client, req)
	}
}

// MapRemoteHostMiddleware [中间件]会对r.RemoteAddr IP地址执行反向DNS查找
func MapRemoteHostMiddleware(inner RequestHandler) RequestHandler {
	return func(client Client, req *Request) (*ResponsePipe, error) {
		r := req.Raw
		remoteAddr, _, _ := net.SplitHostPort(r.RemoteAddr)
		// 根據地址查找到地址的映射列表
		names, _ := net.LookupAddr(remoteAddr)
		if len(names) > 0 {
			// 去除符号"."后填充到req里
			req.Params["REMOTE_HOST"] = strings.TrimRight(names[0], ".")
		}
		return inner(client, req)
	}
}

// FileSystemRouter 有助于生成用于映射路径相关fastcgi参数的中间件实现
type FileSystemRouter struct {

	// DocRoot 存储Apache DocumentRoot参数
	DocRoot string

	// Exts 存储可接受的扩展
	Exts []string

	// DirIndex 存储Apache DirectoryIndex参数，用于标识要在目录中显示的文件
	DirIndex []string
}

// Router 返回一个中间件，用于准备与路径相关的参数
// 通过 FileSystemRouter 中提供的信息，它将请求路由到脚本文件，该文件的路径与http请求路径匹配
//
// i.e. classic PHP hosting environment like Apache + mod_php
// Parameters included:
//  PATH_INFO
//  PATH_TRANSLATED
//  SCRIPT_NAME
//  SCRIPT_FILENAME
//  DOCUMENT_URI
//  DOCUMENT_ROOT
//
func (fs *FileSystemRouter) Router() Middleware {
	return func(inner RequestHandler) RequestHandler {
		return func(client Client, req *Request) (*ResponsePipe, error) {

			// 通过给定的request请求，定义cgi需要的参数
			r := req.Raw
			// 当前脚本的路径
			fastcgiScriptName := r.URL.Path
			// 请求路径信息
			var fastcgiPathInfo string
			// 全局正则表达式变量的安全初始化
			pathinfoRe := regexp.MustCompile(`^(.+\.php)(/?.+)$`)
			// 查找子串
			if matches := pathinfoRe.FindStringSubmatch(fastcgiScriptName); len(matches) > 0 {
				fastcgiScriptName, fastcgiPathInfo = matches[1], matches[2]
			}
			// 判断是否有后缀"/"，如果包含则添加默认index.php
			if strings.HasSuffix(fastcgiScriptName, "/") {
				fastcgiScriptName = path.Join(fastcgiScriptName, "index.php")
			}
			// 包含由客户端提供的、跟在真实脚本名称之后并且在查询语句（query string）之前的路径信息
			req.Params["PATH_INFO"] = fastcgiPathInfo
			// 当前脚本所在文件系统（非文档根目录）的基本路径
			// req.Params["PATH_TRANSLATED"] = filepath.Join(fs.DocRoot, fastcgiPathInfo)
			req.Params["PATH_TRANSLATED"] = filepath.Join(fs.DocRoot, fastcgiScriptName)
			// 包含当前脚本的路径
			req.Params["SCRIPT_NAME"] = fastcgiScriptName
			// 当前执行脚本的绝对路径
			req.Params["SCRIPT_FILENAME"] = filepath.Join(fs.DocRoot, fastcgiScriptName)
			// 请求文档路径
			req.Params["DOCUMENT_URI"] = r.URL.Path
			// 当前运行脚本所在的文档根目录
			req.Params["DOCUMENT_ROOT"] = fs.DocRoot

			return inner(client, req)
		}
	}
}

// MapHeaderMiddleware [中间件]映射header字段（HTTP_*）
// 将header字段xxx-sss映射成HTTP_XXX_SSS
// 注意：无法覆盖HTTP_CONTENT_TYPE和HTTP_CONTENT_LENGTH
func MapHeaderMiddleware(inner RequestHandler) RequestHandler {
	return func(client Client, req *Request) (*ResponsePipe, error) {
		// 获取原始请求
		r := req.Raw
		// 遍历header处理
		for k, v := range r.Header {
			// 转为大写后替换"-"为"_"
			formattedKey := strings.Replace(strings.ToUpper(k), "-", "_", -1)
			if formattedKey == "CONTENT_TYPE" || formattedKey == "CONTENT_LENGTH" {
				continue
			}
			// 加入前缀
			key := "HTTP_" + formattedKey
			var value string
			if len(v) > 0 {
				// 如果一个键对应有多个值，则用,连起来
				//   refer to https://tools.ietf.org/html/rfc7230#section-3.2.2
				//
				//   A recipient MAY combine multiple header fields with the same field
				//   name into one "field-name: field-value" pair, without changing the
				//   semantics of the message, by appending each subsequent field value to
				//   the combined field value in order, separated by a comma.  The order
				//   in which header fields with the same field name are received is
				//   therefore significant to the interpretation of the combined field
				//   value; a proxy MUST NOT change the order of these field values when
				//   forwarding a message.
				value = strings.Join(v, ",")
			}
			// 写入req
			req.Params[key] = value
		}

		return inner(client, req)
	}
}

// MapEndpoint 返回一个中间件，该中间件为应用程序准备RequestHandler
// 以一个文件作为端点（即它将自己处理脚本路由），适用于基于web.py的应用程序
// Parameters included:
//  PATH_INFO
//  PATH_TRANSLATED
//  SCRIPT_NAME
//  SCRIPT_FILENAME
//  DOCUMENT_URI
//  DOCUMENT_ROOT
//
func MapEndpoint(endpointFile string) Middleware {
	dir, webpath := filepath.Dir(endpointFile), "/"+filepath.Base(endpointFile)
	return func(inner RequestHandler) RequestHandler {
		return func(client Client, req *Request) (*ResponsePipe, error) {
			r := req.Raw
			req.Params["REQUEST_URI"] = r.URL.RequestURI()
			req.Params["SCRIPT_NAME"] = webpath
			req.Params["SCRIPT_FILENAME"] = endpointFile
			req.Params["DOCUMENT_URI"] = r.URL.Path
			req.Params["DOCUMENT_ROOT"] = dir
			return inner(client, req)
		}
	}
}

// NewPHPFS 通过连接BasicParamsMapMiddleware,MapHeaderMiddleware和FileSystemRouter，返回PHP请求所需的中间件
func NewPHPFS(root string) Middleware {
	fs := &FileSystemRouter{
		DocRoot:  root,            // DocumentRoot
		Exts:     []string{"php"}, // 脚本文件后缀
		DirIndex: []string{"index.php"},
	}
	return Chain(
		BasicParamsMapMiddleware, // 基础参数映射中间件
		MapHeaderMiddleware,      // 映射header字段中间件（HTTP_*）
		fs.Router(),              // 路由中间件
	)
}

// NewFileEndpoint 通过连接BasicParamsMapMiddleware,MapHeaderMiddleware和MapEndpoint，返回Py请求所需的中间件
func NewFileEndpoint(endpointFile string) Middleware {
	return Chain(
		BasicParamsMapMiddleware, // 基础参数映射中间件
		MapHeaderMiddleware,      // 映射header字段中间件（HTTP_*）
		MapEndpoint(endpointFile),
	)
}
