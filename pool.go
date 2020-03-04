package ffcgiclient

import (
	"time"
)

// PoolClient 继承Client并修改Close方法，用于支持Client池的返回/销毁
type PoolClient struct {
	Client                     // 继承Client
	Err     error              // 错误
	pool    chan<- *PoolClient // 存放PoolClient的通道池，即所属的pool池
	poolTag chan<- uint        // pool标识
	expires time.Time          // 过期时间
}

// Expired 检查是否过期
func (pc *PoolClient) Expired() bool {
	// 如果t代表的时间点在u之后，返回真；否则返回假
	// 测试
	// fmt.Println(time.Now(), "-------", pc.expires)
	return time.Now().After(pc.expires)
}

// Close 仅在内部客户端过期时才关闭它，否则它将自己返回到池中
func (pc *PoolClient) Close() error {
	// 测试
	// 过期则回收
	if pc.Expired() {
		// fmt.Println("【Close】关闭Client")
		return pc.Client.Close()
	}
	go func() {
		// fmt.Println("【Close】放回连接池")
		// 关闭连接
		pc.CloseConn()
		// 阻塞直至返回Client
		pc.poolTag <- 1
		pc.pool <- pc
	}()
	return nil
}

// NewClientPool 创建*ClientPool
// 借助给定的工厂方法创建Client，并将其带有效期地汇集放进*ClientPool中
func NewClientPool(
	clientFactory ClientFactory,
	scale int,
	expires time.Duration,
) *ClientPool {
	// 初始化通道池
	pool := make(chan *PoolClient, scale)
	poolTag := make(chan uint, scale)
	// 开启一个并发协程处理Client创建任务
	go func() {
		for {
			// fmt.Println("【NewClientPool】poolTag <- 1,num:", len(poolTag))
			poolTag <- 1
			// 测试
			// fmt.Println("【NewClientPool】创建ClientPool，有效期：", time.Now().Add(expires))
			// 创建Client
			c, err := clientFactory()
			// 初始化PoolClient，将Client包装为PoolClient
			pc := &PoolClient{
				Client:  c,
				Err:     err,
				pool:    pool,
				poolTag: poolTag,
				expires: time.Now().Add(expires),
			}
			// 放入通道池
			pool <- pc
		}
	}()
	// 返回ClientPool
	return &ClientPool{
		pool:    pool,
		poolTag: poolTag,
	}
}

// ClientPool Client池定义
type ClientPool struct {
	pool    <-chan *PoolClient // 存放PoolClient的通道池
	poolTag <-chan uint
}

// CreateClient 通道池创建Client的工厂方法，需实现ClientFactory类型
func (p *ClientPool) CreateClient() (c Client, err error) {
	// 测试
	// fmt.Println("【CreateClient】从pool中取出一个PoolClient")
	// 从pool中取出一个PoolClient
	pc := <-p.pool
	// 建立连接
	pc.NewConn()
	// 释放
	// fmt.Println("【NewClientPool】<-poolTag,num:", len(p.poolTag))
	<-p.poolTag
	// 检查是否发生错误
	if c, err = pc, pc.Err; err != nil {
		return nil, err
	}
	// 返回
	return
}
