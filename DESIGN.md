# EventBus - 通用跨服务事件总线

## 1. 背景与动机

### 1.1 现有问题

在典型的 Go 游戏服务器项目中，服务间的事件通知通常面临以下问题：

**进程内事件系统的局限**

```go
// 常见的单进程事件系统
var eventMap = make(map[int32][]func(any))

func Register(eventId int32, handler func(any)) { ... }
func Notify(eventId int32, data any)            { ... }
```

- **类型不安全**：handler 签名是 `func(any)`，事件数据类型在运行时才能确定，编译期无法发现类型错误
- **ID 人工管理**：事件用 `int32` 常量标识，需要手动维护不冲突的 ID 列表，随着事件增多极易出错
- **单进程限制**：handler 只在注册它的进程内执行，无法跨服务传播

**跨服务通知靠手写 RPC**

```go
// 每个跨服务通知都要手动编写
event.Notify(PushUpdateAllianceMembers, data)
// → handler 内部:
service.PushMulti(uids, command, msg)
// → 内部再调:
rpclib.Get(rpclib.SeaMapServer, 0).Call("MapServer.PushClients", &in, &out)
```

- 每个需要跨服通知的事件都要**手写 RPC 调用代码**
- 没有统一的"跨服务事件"概念，逻辑散落在各个 handler 中
- 服务间只能单向推送，缺乏对等的双向通知机制
- 多实例场景下实例间互不感知

### 1.2 设计目标

| 目标 | 说明 |
|------|------|
| **类型安全** | 利用 Go 泛型，编译期保证事件数据类型正确 |
| **类型即事件** | 事件的 Go 类型本身就是标识，无需维护 ID 常量表 |
| **跨服务透明** | 本地发布的事件可自动传播到其他服务，订阅方无需关心来源 |
| **传输层可插拔** | 内置 In-Memory 实现；Redis Pub/Sub、NATS 等作为可选传输层 |
| **零强依赖** | 核心模块不依赖任何第三方库；Redis 等传输层作为独立 sub-module |
| **API 简洁** | 三个核心函数：`Subscribe`、`Publish`、`New` |

---

## 2. 核心概念

### 2.1 Event —— 类型即事件

每个事件是一个实现了 `Event` 接口的 Go struct：

```go
type Event interface {
    Topic() string
}
```

事件定义示例：

```go
// 在共享包中定义，所有服务 import 同一个包
package events

type UserLoginEvent struct {
    Uid       int64
    Timestamp int64
    IP        string
}

func (UserLoginEvent) Topic() string { return "user.login" }

type OrderFinishedEvent struct {
    OrderId int64
    Uid     int64
    Amount  int64
}

func (OrderFinishedEvent) Topic() string { return "order.finished" }
```

**设计决策：为什么用 `Topic() string` 而不是用反射取类型名？**

- 显式优于隐式 —— topic 字符串是人类可读的、可 grep 的
- 跨服务一致性 —— 不受 package path、module path 影响
- 可以自由命名 —— 支持 `"user.login"`、`"alliance.member.update"` 等层级命名
- 编译期约束 —— 未实现 `Topic()` 的 struct 无法作为事件发布

### 2.2 Bus —— 事件总线

```go
type Bus struct {
    id        string          // 实例唯一标识（UUID），用于跨服务消息去重
    codec     Codec           // 序列化器
    transport Transport       // 传输层（可选）
    // ... internal state
}
```

- 每个服务进程创建一个 Bus 实例
- 无 Transport 时退化为纯本地事件总线
- 有 Transport 时自动获得跨服务能力

### 2.3 Transport —— 传输层

```go
type Transport interface {
    // 将消息发布到指定 topic
    Publish(ctx context.Context, topic string, data []byte) error

    // 订阅指定 topic，收到消息时调用 handler
    Subscribe(topic string, handler func(ctx context.Context, data []byte)) error

    // 关闭传输层
    Close() error
}
```

Transport 只负责搬运 `[]byte`，不关心序列化细节。

### 2.4 Codec —— 序列化

```go
type Codec interface {
    Marshal(v any) ([]byte, error)
    Unmarshal(data []byte, v any) error
}
```

默认提供 JSON 实现，可替换为 Protobuf、MsgPack 等。

---

## 3. API 详解

### 3.1 创建 Bus

```go
// 最简用法：纯本地事件总线
bus := eventbus.New()

// 带 Redis 跨服务能力
bus := eventbus.New(
    eventbus.WithTransport(redistransport.New(redisClient)),
)

// 自定义序列化
bus := eventbus.New(
    eventbus.WithTransport(redistransport.New(redisClient)),
    eventbus.WithCodec(protobufCodec),
)
```

### 3.2 订阅事件

```go
// 订阅 —— 同时接收本地和远程事件
eventbus.Subscribe(bus, func(ctx context.Context, ev UserLoginEvent) error {
    log.Printf("user %d logged in", ev.Uid)
    return nil
})

// 仅订阅本地事件（不接收其他服务发来的）
eventbus.SubscribeLocal(bus, func(ctx context.Context, ev UserLoginEvent) error {
    // 只有本进程 Publish 的事件会触发这个 handler
    return nil
})
```

**类型安全保证**：

```go
// 编译错误 —— int 没有实现 Event 接口
eventbus.Subscribe(bus, func(ctx context.Context, ev int) error { ... })

// 编译错误 —— handler 签名不匹配
eventbus.Subscribe(bus, func(ev UserLoginEvent) { ... })
```

同一事件可以注册多个 handler，按注册顺序执行：

```go
eventbus.Subscribe(bus, handleLogin)
eventbus.Subscribe(bus, sendWelcomeEmail)
eventbus.Subscribe(bus, updateStatistics)
```

### 3.3 发布事件

```go
// 同步发布：等待所有 handler 执行完毕后返回
// 本地 handler 直接调用 + 序列化后发送到 Transport
err := eventbus.Publish(ctx, bus, UserLoginEvent{Uid: 123})

// 异步发布：handler 在独立 goroutine 中执行，立即返回
eventbus.PublishAsync(ctx, bus, UserLoginEvent{Uid: 123})

// 仅本地发布：不经过 Transport，不传播到其他服务
eventbus.PublishLocal(ctx, bus, UserLoginEvent{Uid: 123})
```

### 3.4 关闭

```go
bus.Close() // 关闭 Transport 连接，释放资源
```

---

## 4. 内部机制

### 4.1 消息流转 —— Publish

```
调用 Publish[T](ctx, bus, event)
│
├── ① 本地分发（零序列化开销）
│   遍历 localHandlers[topic]，直接传递 T 类型对象:
│   for _, h := range handlers {
│       h(ctx, event)    // event 是 T 类型，直接传递，不经过序列化
│   }
│
└── ② 远程分发（仅当 Transport 不为 nil）
    a. payload := codec.Marshal(event)
    b. envelope := envelope{Origin: bus.id, Payload: payload}
    c. data := json.Marshal(envelope)
    d. transport.Publish(ctx, topic, data)
```

**关键优化**：本地 handler 收到的是原始 Go 对象，不经过任何序列化/反序列化。序列化只发生在跨服务传输时。

### 4.2 消息流转 —— 远程接收

```
Transport 收到远程消息 (topic, data []byte)
│
├── 解析 envelope: {Origin, Payload}
├── 检查 Origin == bus.id → 丢弃（自己发的不重复处理）
└── 遍历 remoteHandlers[topic]:
    a. var event T
    b. codec.Unmarshal(envelope.Payload, &event)
    c. handler(ctx, event)
```

### 4.3 类型恢复机制

Go 的泛型函数不能作为方法，所以 `Subscribe` 和 `Publish` 是包级泛型函数。
类型恢复通过**闭包捕获**实现，无需反射：

```go
func Subscribe[T Event](bus *Bus, handler func(context.Context, T) error) {
    var zero T
    topic := zero.Topic()

    // 本地 handler：直接类型断言
    bus.addLocalHandler(topic, func(ctx context.Context, v any) error {
        return handler(ctx, v.(T))
    })

    // 远程 handler：闭包内捕获 T，知道如何反序列化
    bus.addRemoteHandler(topic, func(ctx context.Context, payload []byte) error {
        var event T
        if err := bus.codec.Unmarshal(payload, &event); err != nil {
            return err
        }
        return handler(ctx, event)
    })
}
```

当远程消息到达时，不需要全局的类型注册表。每个 `Subscribe` 调用生成的闭包**自带**反序列化能力。

### 4.4 自身消息去重

每个 Bus 实例在创建时生成一个 UUID 作为 `id`。
消息通过 Transport 传播时，携带发送方的 `id`：

```go
type envelope struct {
    Origin  string `json:"o"` // 发送方 Bus ID
    Payload []byte `json:"p"` // 序列化后的事件数据
}
```

接收方检查 `Origin == bus.id`，相同则丢弃，避免自己处理自己发布的事件（本地 handler 已经在 Publish 时直接调用过了）。

### 4.5 并发安全

- handler 列表使用 `sync.RWMutex` 保护
- Publish 时持读锁遍历 handlers，Subscribe 时持写锁追加 handler
- Transport 的并发安全由各实现方自行保证

---

## 5. Transport 实现

### 5.1 In-Memory Transport

用于单进程场景和单元测试。多个 Bus 实例共享同一个 MemoryTransport 即可互通：

```go
mt := eventbus.NewMemoryTransport()

busA := eventbus.New(eventbus.WithTransport(mt))
busB := eventbus.New(eventbus.WithTransport(mt))

// busA 发布的事件，busB 可以收到（反之亦然）
```

实现要点：
- 内部维护 `map[string][]handler`
- Publish 时遍历对应 topic 的所有 handler 并调用
- 无网络开销，适合测试

### 5.2 Redis Pub/Sub Transport

作为独立 sub-module `eventbus/redistransport`，依赖 `github.com/redis/go-redis/v9`：

```go
rt := redistransport.New(redisClient,
    redistransport.WithPrefix("myapp:events:"),  // channel 前缀
)
bus := eventbus.New(eventbus.WithTransport(rt))
```

实现要点：
- Publish → `redis.Publish(ctx, prefix+topic, data)`
- Subscribe → 动态 `redis.Subscribe(ctx, prefix+topic)`
- 内部启动 goroutine 持续读取消息并分发
- Close 时取消订阅并关闭 goroutine
- 特性：低延迟、无持久化（订阅方掉线会丢消息）

### 5.3 未来可扩展

- **NATS Transport**：适合高吞吐场景
- **Redis Stream Transport**：需要消息持久化和消费者组时
- **Kafka Transport**：需要强持久化和分区有序时

新增 Transport 只需实现三个方法，不影响已有代码。

---

## 6. Module 结构

```
eventbus/
├── go.mod                     // module github.com/xxx/eventbus (零外部依赖)
├── go.sum
│
│   ── 核心 ──
├── event.go                   // Event 接口定义
├── codec.go                   // Codec 接口 + JSONCodec 默认实现
├── transport.go               // Transport 接口定义
├── options.go                 // Option 模式: WithTransport(), WithCodec()
├── bus.go                     // Bus 结构体, New(), Close()
├── subscribe.go               // Subscribe[T](), SubscribeLocal[T]()
├── publish.go                 // Publish[T](), PublishAsync[T](), PublishLocal[T]()
│
│   ── 内置 Transport ──
├── memory.go                  // MemoryTransport（测试 / 单进程）
│
│   ── 测试 ──
├── bus_test.go                // 核心功能测试
├── example_test.go            // 可运行的文档示例
│
│   ── Redis Transport（独立 sub-module）──
└── redistransport/
    ├── go.mod                 // 依赖 go-redis/v9
    ├── go.sum
    ├── redis.go               // RedisTransport 实现
    └── redis_test.go          // 集成测试
```

**为什么 `redistransport` 是独立 sub-module？**

核心模块零外部依赖。不使用 Redis 的项目不会被迫引入 `go-redis`。
使用方按需 `go get github.com/xxx/eventbus/redistransport`。

---

## 7. 使用场景对照

### 7.1 替代进程内事件系统

**Before（类型不安全）：**

```go
const UserLogin = 999998

event.Register(UserLogin, func(data any) {
    uid := data.(int64)  // 运行时才发现类型错误
    // ...
})

event.Notify(UserLogin, uid)
```

**After（编译期类型安全）：**

```go
type UserLoginEvent struct {
    Uid int64
}
func (UserLoginEvent) Topic() string { return "user.login" }

eventbus.Subscribe(bus, func(ctx context.Context, ev UserLoginEvent) error {
    // ev.Uid 编译期确定是 int64
    return nil
})

eventbus.Publish(ctx, bus, UserLoginEvent{Uid: uid})
```

### 7.2 替代手写跨服务 RPC 推送

**Before（手写 RPC）：**

```go
// seaapiserver 中
event.Notify(PushUpdateAllianceMembers, allianceInfo)
// → handler 中:
service.PushMulti(uids, command, msg)
// → 内部:
rpclib.Get(rpclib.SeaMapServer, 0).Call("MapServer.PushClients", &in, &out)
```

**After（透明跨服务）：**

```go
// seaapiserver 中
eventbus.Publish(ctx, bus, AllianceMemberUpdateEvent{
    AllianceId: allianceId,
    Members:    members,
})

// seamapserver 中（自动收到，无需手写 RPC）
eventbus.Subscribe(bus, func(ctx context.Context, ev AllianceMemberUpdateEvent) error {
    pushToClients(ev.Members, ev)
    return nil
})
```

### 7.3 多实例间同步

```
seaapiserver 实例 A ──→ Redis Pub/Sub ──→ seaapiserver 实例 B
                                     └──→ seamapserver 实例 1
                                     └──→ seamapserver 实例 2
```

所有服务的 Bus 使用同一个 Redis Transport，自动互通。

---

## 8. 设计权衡与决策记录

### Q1: 为什么 Subscribe/Publish 是包级函数而不是 Bus 的方法？

Go 的方法不能有独立的类型参数。只有包级函数才能写成 `Subscribe[T Event](...)`。
这是 Go 泛型的语言限制，不是设计选择。

### Q2: 消息丢失怎么办？

Redis Pub/Sub 是 at-most-once 投递。订阅方掉线时消息会丢失。
这在游戏场景下通常可接受 —— 客户端重连时会拉取全量状态。

如果需要 at-least-once，切换到 Redis Stream Transport（未来实现）即可，
业务代码不需要改动，只需替换 Transport。

### Q3: 事件定义放在哪？

事件 struct 应该定义在**所有相关服务都能 import 的共享包**中。
类似于 protobuf 定义文件，事件定义是服务间的契约。

### Q4: handler 的错误怎么处理？

- `Publish`（同步）：任一 handler 返回 error，Publish 返回该 error，但**不中断**后续 handler 的执行
- `PublishAsync`（异步）：error 通过可选的 `ErrorHandler` 回调上报
- Transport 发送失败：Publish 返回 error，但本地 handler 已经执行完毕

### Q5: 性能如何？

- 本地发布：直接函数调用 + 类型断言，无序列化开销，接近原生函数调用性能
- 跨服务发布：一次 JSON Marshal + 一次 Redis Publish，延迟取决于 Redis RTT
- handler 查找：`map[string][]handler`，O(1) topic 查找

### Q6: 与现有系统如何共存？

可以渐进式迁移：
1. 先在新功能中使用 eventbus
2. 旧的 `event.Notify` 和 `eventbus.Publish` 可以共存
3. 逐步将旧事件迁移到新系统，每次迁移一个事件类型
