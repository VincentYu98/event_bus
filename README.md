# EventBus

Go 泛型事件总线库 —— 类型安全、跨服务透明、零外部依赖。

```go
bus := eventbus.New()

sub := eventbus.Subscribe(bus, func(ctx context.Context, ev UserLoginEvent) error {
    log.Printf("user %d logged in", ev.Uid)
    return nil
})
defer sub.Close() // 不再需要时退订

eventbus.Publish(ctx, bus, UserLoginEvent{Uid: 123})
```

## 特性

- **类型安全** — 泛型 API，事件类型在编译期检查，告别 `func(any)`
- **类型即事件** — Go struct 本身就是事件标识，无需维护 ID 常量表
- **跨服务透明** — 配置 Transport 后，本地发布的事件自动传播到其他服务
- **传输层可插拔** — 内置 MemoryTransport；Redis / NATS 等作为独立 sub-module
- **零外部依赖** — 核心模块仅使用标准库
- **本地零序列化** — 本地 handler 直接接收 Go 对象，无序列化开销
- **生命周期管理** — 支持退订（`Subscription.Close()`）和 Bus 关闭后拒绝新操作
- **二进制 Codec** — 传输层 Payload 为 `[]byte`，Protobuf / MsgPack 等二进制编解码器可直接使用

## 安装

```bash
go get github.com/vincentAlen/eventbus
```

要求 Go 1.21+。

## 快速上手

### 1. 定义事件

事件是实现了 `Event` 接口的 struct。`Topic()` 必须使用**值接收者**。

```go
type UserLoginEvent struct {
    Uid       int64  `json:"uid"`
    Timestamp int64  `json:"timestamp"`
}

func (UserLoginEvent) Topic() string { return "user.login" }
```

### 2. 创建 Bus 并订阅

`Subscribe` / `SubscribeLocal` 返回 `*Subscription`，可用于退订。

```go
bus := eventbus.New()

// 订阅：同时接收本地和远程事件
sub := eventbus.Subscribe(bus, func(ctx context.Context, ev UserLoginEvent) error {
    fmt.Printf("user %d logged in\n", ev.Uid)
    return nil
})

// 仅订阅本地事件
localSub := eventbus.SubscribeLocal(bus, func(ctx context.Context, ev UserLoginEvent) error {
    // 只有本进程 Publish 的事件会触发
    return nil
})

// 不再需要时退订（Safe to call multiple times or on nil）
sub.Close()
localSub.Close()
```

### 3. 发布事件

```go
// 同步发布：等待所有 handler 执行完毕
err := eventbus.Publish(ctx, bus, UserLoginEvent{Uid: 123})

// 异步发布：立即返回，handler 在 goroutine 中执行
eventbus.PublishAsync(ctx, bus, UserLoginEvent{Uid: 123})

// 仅本地发布：不经过 Transport
err := eventbus.PublishLocal(ctx, bus, UserLoginEvent{Uid: 123})
```

### 4. 跨 Bus 通信

多个 Bus 共享同一个 Transport 即可互通：

```go
mt := eventbus.NewMemoryTransport()

bus1 := eventbus.New(eventbus.WithTransport(mt))
bus2 := eventbus.New(eventbus.WithTransport(mt))

eventbus.Subscribe(bus2, func(ctx context.Context, ev UserLoginEvent) error {
    fmt.Println("bus2 received:", ev.Uid)
    return nil
})

// bus1 发布，bus2 自动收到
eventbus.Publish(ctx, bus1, UserLoginEvent{Uid: 456})
```

## 配置选项

```go
bus := eventbus.New(
    eventbus.WithTransport(transport),    // 设置传输层
    eventbus.WithCodec(myCodec),          // 自定义序列化（默认 JSON，支持二进制 Codec）
    eventbus.WithErrorHandler(func(err error) {  // 异步错误回调
        log.Println("eventbus error:", err)
    }),
    eventbus.WithAsyncLimit(2048),        // PublishAsync 最大并发数（默认 4096）
)
```

## 核心接口与类型

### Event

```go
type Event interface {
    Topic() string
}
```

### Subscription

`Subscribe` / `SubscribeLocal` 的返回值，用于退订。

```go
type Subscription struct{ /* unexported */ }

// Close 移除 handler。可多次调用，nil 安全。
func (s *Subscription) Close()
```

### Transport

```go
type Transport interface {
    Publish(ctx context.Context, topic string, data []byte) error
    Subscribe(topic string, handler func(ctx context.Context, data []byte)) error
    Close() error
}
```

### Codec

`Codec` 输出的 `[]byte` 在传输层自动 base64 编码，因此 Protobuf / MsgPack 等二进制格式可直接使用。

```go
type Codec interface {
    Marshal(v any) ([]byte, error)
    Unmarshal(data []byte, v any) error
}
```

### 哨兵错误

```go
var ErrBusClosed       = errors.New("eventbus: bus closed")
var ErrTransportClosed = errors.New("eventbus: transport closed")
```

## 内部机制

### 消息流转

```
Publish[T](ctx, bus, event)
│
├── ① 本地分发（零序列化）
│   遍历 localHandlers[topic]，直接传递 T 类型对象
│
└── ② 远程分发（仅当 Transport != nil）
    codec.Marshal(event) → envelope{Origin, Payload} → transport.Publish
```

### 自身消息去重

每个 Bus 实例持有唯一 ID。消息通过 Transport 传输时携带发送方 ID（envelope），接收方检查 `Origin == bus.id` 则丢弃，避免本地 handler 重复执行。

### 错误处理

- `Publish`（同步）：所有 handler 均执行，错误通过 `errors.Join` 聚合返回
- `PublishAsync`（异步）：错误通过 `ErrorHandler` 回调上报
- 单个 handler 出错不影响其他 handler 执行
- Transport 订阅失败会通过 `ErrorHandler` 上报，并允许下次 `Subscribe` 重试
- 远程消息 envelope 反序列化失败同样通过 `ErrorHandler` 上报（不再静默丢弃）

### 生命周期

```go
bus := eventbus.New(eventbus.WithTransport(mt))

// ... 使用 bus ...

// 关闭后：Publish/PublishLocal 返回 ErrBusClosed，
// Subscribe 返回 nil，PublishAsync 通过 ErrorHandler 上报错误。
bus.Close()
```

### 背压

`PublishAsync` 通过内置信号量限制并发 goroutine 数量（默认 4096）。达到上限时调用方阻塞，直到有槽位释放，防止 goroutine 膨胀。

```go
// 自定义上限
bus := eventbus.New(eventbus.WithAsyncLimit(1024))
```

## 项目结构

```
eventbus/
├── event.go          Event 接口
├── codec.go          Codec 接口 + JSONCodec
├── transport.go      Transport 接口 + ErrTransportClosed
├── options.go        WithTransport / WithCodec / WithErrorHandler / WithAsyncLimit
├── bus.go            Bus 核心实现 + Subscription 类型
├── subscribe.go      Subscribe[T] / SubscribeLocal[T] → *Subscription
├── publish.go        Publish[T] / PublishAsync[T] / PublishLocal[T]
├── memory.go         MemoryTransport
├── bus_test.go       核心测试（13 cases）
└── example_test.go   可运行文档示例
```

## License

MIT
