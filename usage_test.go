package eventbus_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wepie/eventbus"
)

// ============================================================
// 测试用事件类型
// ============================================================

type UserLogin struct {
	Uid       int64  `json:"uid"`
	IP        string `json:"ip"`
	Timestamp int64  `json:"timestamp"`
}

func (UserLogin) Topic() string { return "user.login" }

type OrderFinished struct {
	OrderID int64 `json:"order_id"`
	Uid     int64 `json:"uid"`
	Amount  int64 `json:"amount"`
}

func (OrderFinished) Topic() string { return "order.finished" }

type ChatSent struct {
	From string `json:"from"`
	To   string `json:"to"`
	Text string `json:"text"`
}

func (ChatSent) Topic() string { return "chat.sent" }

type LargePayload struct {
	Data string `json:"data"`
}

func (LargePayload) Topic() string { return "large.payload" }

// ============================================================
// 自定义 Codec（用于 WithCodec 测试）
// ============================================================

// uppercaseCodec 在 Marshal 时将 JSON 转为大写，Unmarshal 时转回小写。
// 用于验证自定义 Codec 是否真正被调用。
type uppercaseCodec struct {
	inner eventbus.JSONCodec
}

func (c uppercaseCodec) Marshal(v any) ([]byte, error) {
	data, err := c.inner.Marshal(v)
	if err != nil {
		return nil, err
	}
	return []byte(strings.ToUpper(string(data))), nil
}

func (c uppercaseCodec) Unmarshal(data []byte, v any) error {
	return c.inner.Unmarshal([]byte(strings.ToLower(string(data))), v)
}

// ============================================================
// 一、构造与配置
// ============================================================

func TestNew_DefaultOptions(t *testing.T) {
	bus := eventbus.New()
	if bus.ID() == "" {
		t.Fatal("Bus ID should be non-empty")
	}
	// Close without transport should be safe.
	if err := bus.Close(); err != nil {
		t.Fatalf("Close without transport should return nil, got %v", err)
	}
}

func TestNew_UniqueIDs(t *testing.T) {
	ids := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := eventbus.New().ID()
		if _, dup := ids[id]; dup {
			t.Fatalf("duplicate ID at iteration %d: %s", i, id)
		}
		ids[id] = struct{}{}
	}
}

func TestNew_WithAllOptions(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	var errHandlerCalled bool
	bus := eventbus.New(
		eventbus.WithTransport(mt),
		eventbus.WithCodec(uppercaseCodec{}),
		eventbus.WithErrorHandler(func(err error) { errHandlerCalled = true }),
	)
	_ = errHandlerCalled

	if bus.ID() == "" {
		t.Fatal("expected non-empty ID")
	}
	if err := bus.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ============================================================
// 二、Subscribe + Publish —— 本地分发
// ============================================================

func TestSubscribe_LocalDelivery(t *testing.T) {
	bus := eventbus.New()
	var got UserLogin
	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		got = ev
		return nil
	})

	want := UserLogin{Uid: 42, IP: "127.0.0.1", Timestamp: 1700000000}
	if err := eventbus.Publish(context.Background(), bus, want); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSubscribe_MultipleHandlers_ExecutionOrder(t *testing.T) {
	bus := eventbus.New()
	var order []int
	for i := 1; i <= 5; i++ {
		i := i
		eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
			order = append(order, i)
			return nil
		})
	}

	_ = eventbus.Publish(context.Background(), bus, UserLogin{Uid: 1})

	if len(order) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(order))
	}
	for i, v := range order {
		if v != i+1 {
			t.Fatalf("expected order[%d]=%d, got %d", i, i+1, v)
		}
	}
}

func TestSubscribe_MultipleTopics_Isolation(t *testing.T) {
	bus := eventbus.New()
	var loginCount, orderCount atomic.Int32

	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		loginCount.Add(1)
		return nil
	})
	eventbus.Subscribe(bus, func(ctx context.Context, ev OrderFinished) error {
		orderCount.Add(1)
		return nil
	})

	_ = eventbus.Publish(context.Background(), bus, UserLogin{Uid: 1})
	_ = eventbus.Publish(context.Background(), bus, UserLogin{Uid: 2})
	_ = eventbus.Publish(context.Background(), bus, OrderFinished{OrderID: 100})

	if loginCount.Load() != 2 {
		t.Fatalf("expected 2 login events, got %d", loginCount.Load())
	}
	if orderCount.Load() != 1 {
		t.Fatalf("expected 1 order event, got %d", orderCount.Load())
	}
}

// ============================================================
// 三、SubscribeLocal —— 仅本地
// ============================================================

func TestSubscribeLocal_ReceivesLocalPublish(t *testing.T) {
	bus := eventbus.New()
	var got UserLogin
	eventbus.SubscribeLocal(bus, func(ctx context.Context, ev UserLogin) error {
		got = ev
		return nil
	})

	want := UserLogin{Uid: 7}
	_ = eventbus.PublishLocal(context.Background(), bus, want)

	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSubscribeLocal_IgnoresRemoteMessages(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	sender := eventbus.New(eventbus.WithTransport(mt))
	receiver := eventbus.New(eventbus.WithTransport(mt))

	var called atomic.Int32
	eventbus.SubscribeLocal(receiver, func(ctx context.Context, ev UserLogin) error {
		called.Add(1)
		return nil
	})

	// sender 发布，receiver 的 SubscribeLocal handler 不应被触发
	_ = eventbus.Publish(context.Background(), sender, UserLogin{Uid: 99})
	time.Sleep(20 * time.Millisecond)

	if called.Load() != 0 {
		t.Fatalf("SubscribeLocal should not receive remote messages, got %d calls", called.Load())
	}
}

// ============================================================
// 四、PublishLocal —— 仅本地分发，不经过 Transport
// ============================================================

func TestPublishLocal_DoesNotGoThroughTransport(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	bus1 := eventbus.New(eventbus.WithTransport(mt))
	bus2 := eventbus.New(eventbus.WithTransport(mt))

	var bus2Called atomic.Int32
	eventbus.Subscribe(bus2, func(ctx context.Context, ev UserLogin) error {
		bus2Called.Add(1)
		return nil
	})

	// PublishLocal 不走 Transport，bus2 不应收到
	_ = eventbus.PublishLocal(context.Background(), bus1, UserLogin{Uid: 1})
	time.Sleep(20 * time.Millisecond)

	if bus2Called.Load() != 0 {
		t.Fatalf("PublishLocal should not propagate through Transport, got %d calls on bus2", bus2Called.Load())
	}
}

func TestPublishLocal_NoSubscribers_NoError(t *testing.T) {
	bus := eventbus.New()
	err := eventbus.PublishLocal(context.Background(), bus, UserLogin{Uid: 1})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

// ============================================================
// 五、Publish —— 本地 + 远程
// ============================================================

func TestPublish_WithTransport_DeliversBothLocalAndRemote(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	bus1 := eventbus.New(eventbus.WithTransport(mt))
	bus2 := eventbus.New(eventbus.WithTransport(mt))

	var local, remote UserLogin
	eventbus.Subscribe(bus1, func(ctx context.Context, ev UserLogin) error {
		local = ev
		return nil
	})
	eventbus.Subscribe(bus2, func(ctx context.Context, ev UserLogin) error {
		remote = ev
		return nil
	})

	want := UserLogin{Uid: 55, IP: "10.0.0.1"}
	_ = eventbus.Publish(context.Background(), bus1, want)

	if local != want {
		t.Fatalf("local handler: got %+v, want %+v", local, want)
	}
	if remote != want {
		t.Fatalf("remote handler: got %+v, want %+v", remote, want)
	}
}

func TestPublish_SelfDedup(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	bus := eventbus.New(eventbus.WithTransport(mt))

	var count atomic.Int32
	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		count.Add(1)
		return nil
	})

	_ = eventbus.Publish(context.Background(), bus, UserLogin{Uid: 1})

	// 本地调一次，远程回环被去重，总计 1 次
	if count.Load() != 1 {
		t.Fatalf("expected 1 (deduped), got %d", count.Load())
	}
}

func TestPublish_NoTransport_LocalOnly(t *testing.T) {
	bus := eventbus.New() // 无 Transport
	var got UserLogin
	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		got = ev
		return nil
	})

	want := UserLogin{Uid: 8}
	err := eventbus.Publish(context.Background(), bus, want)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// ============================================================
// 六、PublishAsync
// ============================================================

func TestPublishAsync_Delivery(t *testing.T) {
	bus := eventbus.New()
	var wg sync.WaitGroup
	wg.Add(1)

	var got UserLogin
	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		got = ev
		wg.Done()
		return nil
	})

	eventbus.PublishAsync(context.Background(), bus, UserLogin{Uid: 77})
	wg.Wait()

	if got.Uid != 77 {
		t.Fatalf("expected Uid=77, got %d", got.Uid)
	}
}

func TestPublishAsync_ErrorHandler(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)

	var handledErr error
	bus := eventbus.New(eventbus.WithErrorHandler(func(err error) {
		handledErr = err
		wg.Done()
	}))

	errBoom := errors.New("boom")
	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		return errBoom
	})

	eventbus.PublishAsync(context.Background(), bus, UserLogin{Uid: 1})
	wg.Wait()

	if !errors.Is(handledErr, errBoom) {
		t.Fatalf("expected errBoom, got %v", handledErr)
	}
}

func TestPublishAsync_NoErrorHandler_DoesNotPanic(t *testing.T) {
	bus := eventbus.New() // 无 ErrorHandler
	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		return errors.New("ignored")
	})

	// 不应 panic
	eventbus.PublishAsync(context.Background(), bus, UserLogin{Uid: 1})
	time.Sleep(50 * time.Millisecond)
}

// ============================================================
// 七、错误处理
// ============================================================

func TestPublish_HandlerError_AllHandlersStillRun(t *testing.T) {
	bus := eventbus.New()
	err1 := errors.New("err1")
	err2 := errors.New("err2")
	var thirdCalled bool

	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error { return err1 })
	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error { return err2 })
	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		thirdCalled = true
		return nil
	})

	err := eventbus.Publish(context.Background(), bus, UserLogin{Uid: 1})

	if !thirdCalled {
		t.Fatal("third handler should have been called")
	}
	if !errors.Is(err, err1) || !errors.Is(err, err2) {
		t.Fatalf("expected joined errors containing err1 and err2, got %v", err)
	}
}

func TestPublish_TransportError_LocalHandlersAlreadyExecuted(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	bus := eventbus.New(eventbus.WithTransport(mt))

	var localCalled bool
	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		localCalled = true
		return nil
	})

	// 关闭 Transport，使 Publish 的远程部分失败
	_ = mt.Close()

	err := eventbus.Publish(context.Background(), bus, UserLogin{Uid: 1})

	if !localCalled {
		t.Fatal("local handler should have been called even when transport fails")
	}
	if !errors.Is(err, eventbus.ErrTransportClosed) {
		t.Fatalf("expected ErrTransportClosed, got %v", err)
	}
}

// ============================================================
// 八、跨 Bus 通信
// ============================================================

func TestCrossBus_ThreeBuses(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	bus1 := eventbus.New(eventbus.WithTransport(mt))
	bus2 := eventbus.New(eventbus.WithTransport(mt))
	bus3 := eventbus.New(eventbus.WithTransport(mt))

	var count2, count3 atomic.Int32
	eventbus.Subscribe(bus2, func(ctx context.Context, ev ChatSent) error {
		count2.Add(1)
		return nil
	})
	eventbus.Subscribe(bus3, func(ctx context.Context, ev ChatSent) error {
		count3.Add(1)
		return nil
	})

	_ = eventbus.Publish(context.Background(), bus1, ChatSent{From: "A", To: "B", Text: "hello"})

	if count2.Load() != 1 {
		t.Fatalf("bus2 expected 1 call, got %d", count2.Load())
	}
	if count3.Load() != 1 {
		t.Fatalf("bus3 expected 1 call, got %d", count3.Load())
	}
}

func TestCrossBus_BidirectionalCommunication(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	bus1 := eventbus.New(eventbus.WithTransport(mt))
	bus2 := eventbus.New(eventbus.WithTransport(mt))

	var got1, got2 ChatSent
	eventbus.Subscribe(bus1, func(ctx context.Context, ev ChatSent) error {
		got1 = ev
		return nil
	})
	eventbus.Subscribe(bus2, func(ctx context.Context, ev ChatSent) error {
		got2 = ev
		return nil
	})

	// bus1 -> bus2
	_ = eventbus.Publish(context.Background(), bus1, ChatSent{From: "bus1", Text: "ping"})
	if got2.From != "bus1" {
		t.Fatalf("bus2 should receive from bus1, got %+v", got2)
	}

	// bus2 -> bus1
	_ = eventbus.Publish(context.Background(), bus2, ChatSent{From: "bus2", Text: "pong"})
	if got1.From != "bus2" {
		t.Fatalf("bus1 should receive from bus2, got %+v", got1)
	}
}

func TestCrossBus_MultipleTopics(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	bus1 := eventbus.New(eventbus.WithTransport(mt))
	bus2 := eventbus.New(eventbus.WithTransport(mt))

	var loginReceived, orderReceived bool
	eventbus.Subscribe(bus2, func(ctx context.Context, ev UserLogin) error {
		loginReceived = true
		return nil
	})
	eventbus.Subscribe(bus2, func(ctx context.Context, ev OrderFinished) error {
		orderReceived = true
		return nil
	})

	_ = eventbus.Publish(context.Background(), bus1, UserLogin{Uid: 1})
	_ = eventbus.Publish(context.Background(), bus1, OrderFinished{OrderID: 100})

	if !loginReceived {
		t.Fatal("bus2 should receive UserLogin")
	}
	if !orderReceived {
		t.Fatal("bus2 should receive OrderFinished")
	}
}

// ============================================================
// 九、WithCodec —— 自定义序列化
// ============================================================

func TestWithCodec_CustomCodec(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	codec := uppercaseCodec{}

	bus1 := eventbus.New(eventbus.WithTransport(mt), eventbus.WithCodec(codec))
	bus2 := eventbus.New(eventbus.WithTransport(mt), eventbus.WithCodec(codec))

	var got UserLogin
	eventbus.Subscribe(bus2, func(ctx context.Context, ev UserLogin) error {
		got = ev
		return nil
	})

	want := UserLogin{Uid: 123, IP: "192.168.1.1"}
	_ = eventbus.Publish(context.Background(), bus1, want)

	if got != want {
		t.Fatalf("custom codec: got %+v, want %+v", got, want)
	}
}

// ============================================================
// 十、Context 传播
// ============================================================

func TestPublish_ContextPropagation(t *testing.T) {
	bus := eventbus.New()

	type ctxKey string
	key := ctxKey("request_id")

	var gotVal string
	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		gotVal, _ = ctx.Value(key).(string)
		return nil
	})

	ctx := context.WithValue(context.Background(), key, "req-abc-123")
	_ = eventbus.Publish(ctx, bus, UserLogin{Uid: 1})

	if gotVal != "req-abc-123" {
		t.Fatalf("expected context value req-abc-123, got %s", gotVal)
	}
}

// ============================================================
// 十一、大消息体
// ============================================================

func TestPublish_LargePayload(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	bus1 := eventbus.New(eventbus.WithTransport(mt))
	bus2 := eventbus.New(eventbus.WithTransport(mt))

	bigData := strings.Repeat("x", 1<<20) // 1 MB

	var got LargePayload
	eventbus.Subscribe(bus2, func(ctx context.Context, ev LargePayload) error {
		got = ev
		return nil
	})

	_ = eventbus.Publish(context.Background(), bus1, LargePayload{Data: bigData})

	if len(got.Data) != 1<<20 {
		t.Fatalf("expected 1MB payload, got %d bytes", len(got.Data))
	}
}

// ============================================================
// 十二、MemoryTransport 直接测试
// ============================================================

func TestMemoryTransport_PublishSubscribe(t *testing.T) {
	mt := eventbus.NewMemoryTransport()

	var received []byte
	_ = mt.Subscribe("test", func(ctx context.Context, data []byte) {
		received = data
	})

	_ = mt.Publish(context.Background(), "test", []byte("hello"))

	if string(received) != "hello" {
		t.Fatalf("expected hello, got %s", string(received))
	}
}

func TestMemoryTransport_MultipleSubscribers(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	var count atomic.Int32

	for i := 0; i < 5; i++ {
		_ = mt.Subscribe("topic", func(ctx context.Context, data []byte) {
			count.Add(1)
		})
	}

	_ = mt.Publish(context.Background(), "topic", []byte("msg"))

	if count.Load() != 5 {
		t.Fatalf("expected 5, got %d", count.Load())
	}
}

func TestMemoryTransport_CloseBlocksPublish(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	_ = mt.Close()

	err := mt.Publish(context.Background(), "topic", []byte("data"))
	if !errors.Is(err, eventbus.ErrTransportClosed) {
		t.Fatalf("expected ErrTransportClosed, got %v", err)
	}
}

func TestMemoryTransport_CloseBlocksSubscribe(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	_ = mt.Close()

	err := mt.Subscribe("topic", func(ctx context.Context, data []byte) {})
	if !errors.Is(err, eventbus.ErrTransportClosed) {
		t.Fatalf("expected ErrTransportClosed, got %v", err)
	}
}

func TestMemoryTransport_DataCopyIsolation(t *testing.T) {
	mt := eventbus.NewMemoryTransport()

	var received []byte
	_ = mt.Subscribe("topic", func(ctx context.Context, data []byte) {
		received = data
	})

	buf := []byte("original")
	_ = mt.Publish(context.Background(), "topic", buf)

	// 修改原 buffer 不应影响 handler 收到的数据
	buf[0] = 'X'
	if string(received) != "original" {
		t.Fatalf("data should be copied, got %s", string(received))
	}
}

// ============================================================
// 十三、Bus.Close
// ============================================================

func TestClose_WithoutTransport(t *testing.T) {
	bus := eventbus.New()
	if err := bus.Close(); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestClose_WithTransport(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	bus := eventbus.New(eventbus.WithTransport(mt))

	if err := bus.Close(); err != nil {
		t.Fatal(err)
	}

	// Transport 已关闭
	err := mt.Publish(context.Background(), "t", []byte("d"))
	if !errors.Is(err, eventbus.ErrTransportClosed) {
		t.Fatalf("expected ErrTransportClosed after Close, got %v", err)
	}
}

// ============================================================
// 十四、并发安全
// ============================================================

func TestConcurrent_PublishAndSubscribe(t *testing.T) {
	bus := eventbus.New()
	ctx := context.Background()

	var count atomic.Int64
	var wg sync.WaitGroup

	// 并发订阅
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
				count.Add(1)
				return nil
			})
		}()
	}
	wg.Wait()

	// 并发发布
	n := 100
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = eventbus.Publish(ctx, bus, UserLogin{Uid: 1})
		}()
	}
	wg.Wait()

	// 10 个 handler × 100 次发布 = 1000
	if count.Load() != 1000 {
		t.Fatalf("expected 1000, got %d", count.Load())
	}
}

func TestConcurrent_CrossBus(t *testing.T) {
	mt := eventbus.NewMemoryTransport()
	bus1 := eventbus.New(eventbus.WithTransport(mt))
	bus2 := eventbus.New(eventbus.WithTransport(mt))

	var received atomic.Int64
	eventbus.Subscribe(bus2, func(ctx context.Context, ev UserLogin) error {
		received.Add(1)
		return nil
	})

	n := 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(uid int64) {
			defer wg.Done()
			_ = eventbus.Publish(context.Background(), bus1, UserLogin{Uid: uid})
		}(int64(i))
	}
	wg.Wait()

	if received.Load() != int64(n) {
		t.Fatalf("expected %d remote deliveries, got %d", n, received.Load())
	}
}

// ============================================================
// 十五、压力测试 (Stress Tests)
// ============================================================

// TestStress_HighConcurrencyPublish 高并发本地发布
func TestStress_HighConcurrencyPublish(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	bus := eventbus.New()
	var count atomic.Int64

	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		count.Add(1)
		return nil
	})

	const goroutines = 100
	const perGoroutine = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				_ = eventbus.Publish(context.Background(), bus, UserLogin{Uid: int64(i)})
			}
		}()
	}
	wg.Wait()

	expected := int64(goroutines * perGoroutine)
	if count.Load() != expected {
		t.Fatalf("expected %d, got %d", expected, count.Load())
	}
}

// TestStress_CrossBusHighThroughput 高吞吐跨 Bus 发布
func TestStress_CrossBusHighThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	mt := eventbus.NewMemoryTransport()
	sender := eventbus.New(eventbus.WithTransport(mt))
	receiver := eventbus.New(eventbus.WithTransport(mt))

	var received atomic.Int64
	eventbus.Subscribe(receiver, func(ctx context.Context, ev UserLogin) error {
		received.Add(1)
		return nil
	})

	const total = 50000
	var wg sync.WaitGroup
	wg.Add(total)
	for i := 0; i < total; i++ {
		go func(uid int64) {
			defer wg.Done()
			_ = eventbus.Publish(context.Background(), sender, UserLogin{Uid: uid})
		}(int64(i))
	}
	wg.Wait()

	if received.Load() != total {
		t.Fatalf("expected %d, got %d", total, received.Load())
	}
}

// TestStress_ManyTopics 大量 topic 互不干扰
func TestStress_ManyTopics(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	bus := eventbus.New()
	var loginCount, orderCount, chatCount atomic.Int64

	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		loginCount.Add(1)
		return nil
	})
	eventbus.Subscribe(bus, func(ctx context.Context, ev OrderFinished) error {
		orderCount.Add(1)
		return nil
	})
	eventbus.Subscribe(bus, func(ctx context.Context, ev ChatSent) error {
		chatCount.Add(1)
		return nil
	})

	const n = 10000
	var wg sync.WaitGroup
	wg.Add(n * 3)
	for i := 0; i < n; i++ {
		go func() { defer wg.Done(); _ = eventbus.Publish(context.Background(), bus, UserLogin{Uid: 1}) }()
		go func() {
			defer wg.Done()
			_ = eventbus.Publish(context.Background(), bus, OrderFinished{OrderID: 1})
		}()
		go func() {
			defer wg.Done()
			_ = eventbus.Publish(context.Background(), bus, ChatSent{From: "a", To: "b"})
		}()
	}
	wg.Wait()

	if loginCount.Load() != n {
		t.Fatalf("login: expected %d, got %d", n, loginCount.Load())
	}
	if orderCount.Load() != n {
		t.Fatalf("order: expected %d, got %d", n, orderCount.Load())
	}
	if chatCount.Load() != n {
		t.Fatalf("chat: expected %d, got %d", n, chatCount.Load())
	}
}

// TestStress_ManyBusesOneTransport 多 Bus 共享一个 Transport
func TestStress_ManyBusesOneTransport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	mt := eventbus.NewMemoryTransport()
	const numBuses = 20
	buses := make([]*eventbus.Bus, numBuses)
	counters := make([]atomic.Int64, numBuses)

	for i := 0; i < numBuses; i++ {
		buses[i] = eventbus.New(eventbus.WithTransport(mt))
		idx := i
		eventbus.Subscribe(buses[i], func(ctx context.Context, ev UserLogin) error {
			counters[idx].Add(1)
			return nil
		})
	}

	// bus[0] 发布 1000 条消息
	const msgs = 1000
	var wg sync.WaitGroup
	wg.Add(msgs)
	for i := 0; i < msgs; i++ {
		go func(uid int64) {
			defer wg.Done()
			_ = eventbus.Publish(context.Background(), buses[0], UserLogin{Uid: uid})
		}(int64(i))
	}
	wg.Wait()

	// bus[0] 本地接收 1000 次（自身去重）
	if counters[0].Load() != msgs {
		t.Fatalf("bus[0] (sender) expected %d, got %d", msgs, counters[0].Load())
	}
	// 其余 bus 各收到 1000 次远程消息
	for i := 1; i < numBuses; i++ {
		if counters[i].Load() != msgs {
			t.Fatalf("bus[%d] expected %d, got %d", i, msgs, counters[i].Load())
		}
	}
}

// TestStress_ConcurrentSubscribeAndPublish 并发注册 + 并发发布
func TestStress_ConcurrentSubscribeAndPublish(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	bus := eventbus.New()
	ctx := context.Background()

	var totalCalls atomic.Int64

	var wg sync.WaitGroup

	// 持续注册 handler
	const subscribers = 50
	wg.Add(subscribers)
	for s := 0; s < subscribers; s++ {
		go func() {
			defer wg.Done()
			eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
				totalCalls.Add(1)
				return nil
			})
		}()
	}

	// 同时持续发布
	const publishers = 50
	const msgsPerPub = 100
	wg.Add(publishers)
	for p := 0; p < publishers; p++ {
		go func() {
			defer wg.Done()
			for i := 0; i < msgsPerPub; i++ {
				_ = eventbus.Publish(ctx, bus, UserLogin{Uid: int64(i)})
			}
		}()
	}

	wg.Wait()

	// 由于 subscribe 和 publish 并发，handler 数量在发布时可能还没注册完，
	// 只验证不 panic 不死锁，且调用次数 > 0
	if totalCalls.Load() == 0 {
		t.Fatal("expected some handler calls, got 0")
	}
	t.Logf("total handler calls: %d", totalCalls.Load())
}

// ============================================================
// 十六、Benchmark 基准测试
// ============================================================

func BenchmarkPublishLocal_SingleHandler(b *testing.B) {
	bus := eventbus.New()
	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		return nil
	})
	ctx := context.Background()
	ev := UserLogin{Uid: 1, IP: "127.0.0.1"}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = eventbus.PublishLocal(ctx, bus, ev)
	}
}

func BenchmarkPublishLocal_10Handlers(b *testing.B) {
	bus := eventbus.New()
	for i := 0; i < 10; i++ {
		eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
			return nil
		})
	}
	ctx := context.Background()
	ev := UserLogin{Uid: 1}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = eventbus.PublishLocal(ctx, bus, ev)
	}
}

func BenchmarkPublish_WithTransport(b *testing.B) {
	mt := eventbus.NewMemoryTransport()
	bus := eventbus.New(eventbus.WithTransport(mt))
	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		return nil
	})
	ctx := context.Background()
	ev := UserLogin{Uid: 1, IP: "10.0.0.1"}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = eventbus.Publish(ctx, bus, ev)
	}
}

func BenchmarkPublish_CrossBus(b *testing.B) {
	mt := eventbus.NewMemoryTransport()
	sender := eventbus.New(eventbus.WithTransport(mt))
	receiver := eventbus.New(eventbus.WithTransport(mt))
	eventbus.Subscribe(receiver, func(ctx context.Context, ev UserLogin) error {
		return nil
	})
	ctx := context.Background()
	ev := UserLogin{Uid: 1}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = eventbus.Publish(ctx, sender, ev)
	}
}

func BenchmarkPublishLocal_Parallel(b *testing.B) {
	bus := eventbus.New()
	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
		return nil
	})
	ctx := context.Background()
	ev := UserLogin{Uid: 1}

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = eventbus.PublishLocal(ctx, bus, ev)
		}
	})
}

func BenchmarkPublish_CrossBus_Parallel(b *testing.B) {
	mt := eventbus.NewMemoryTransport()
	sender := eventbus.New(eventbus.WithTransport(mt))
	receiver := eventbus.New(eventbus.WithTransport(mt))
	eventbus.Subscribe(receiver, func(ctx context.Context, ev UserLogin) error {
		return nil
	})
	ctx := context.Background()
	ev := UserLogin{Uid: 1}

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = eventbus.Publish(ctx, sender, ev)
		}
	})
}

func BenchmarkSubscribe(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bus := eventbus.New()
		eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error {
			return nil
		})
	}
}

func BenchmarkPublish_LargePayload(b *testing.B) {
	mt := eventbus.NewMemoryTransport()
	sender := eventbus.New(eventbus.WithTransport(mt))
	receiver := eventbus.New(eventbus.WithTransport(mt))
	eventbus.Subscribe(receiver, func(ctx context.Context, ev LargePayload) error {
		return nil
	})
	ctx := context.Background()
	ev := LargePayload{Data: strings.Repeat("a", 4096)} // 4 KB

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = eventbus.Publish(ctx, sender, ev)
	}
}

func BenchmarkPublish_ManyTopics(b *testing.B) {
	bus := eventbus.New()
	eventbus.Subscribe(bus, func(ctx context.Context, ev UserLogin) error { return nil })
	eventbus.Subscribe(bus, func(ctx context.Context, ev OrderFinished) error { return nil })
	eventbus.Subscribe(bus, func(ctx context.Context, ev ChatSent) error { return nil })
	ctx := context.Background()

	events := []func(){
		func() { _ = eventbus.PublishLocal(ctx, bus, UserLogin{Uid: 1}) },
		func() { _ = eventbus.PublishLocal(ctx, bus, OrderFinished{OrderID: 1}) },
		func() { _ = eventbus.PublishLocal(ctx, bus, ChatSent{From: "a"}) },
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		events[i%3]()
	}
}

func BenchmarkPublish_ManyBuses(b *testing.B) {
	mt := eventbus.NewMemoryTransport()
	sender := eventbus.New(eventbus.WithTransport(mt))

	const numReceivers = 10
	for i := 0; i < numReceivers; i++ {
		r := eventbus.New(eventbus.WithTransport(mt))
		eventbus.Subscribe(r, func(ctx context.Context, ev UserLogin) error { return nil })
	}

	ctx := context.Background()
	ev := UserLogin{Uid: 1}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = eventbus.Publish(ctx, sender, ev)
	}
}

// ============================================================
// 十七、综合场景：模拟真实业务流
// ============================================================

func TestScenario_GameServerEventFlow(t *testing.T) {
	mt := eventbus.NewMemoryTransport()

	// 模拟三个服务
	apiServer := eventbus.New(eventbus.WithTransport(mt))
	gameServer := eventbus.New(eventbus.WithTransport(mt))
	logServer := eventbus.New(eventbus.WithTransport(mt))

	var (
		gameNotified  atomic.Int32
		logRecorded   atomic.Int32
		localAudit    atomic.Int32
		orderNotified atomic.Int32
	)

	// gameServer 订阅 UserLogin
	eventbus.Subscribe(gameServer, func(ctx context.Context, ev UserLogin) error {
		gameNotified.Add(1)
		return nil
	})

	// logServer 订阅 UserLogin 和 OrderFinished
	eventbus.Subscribe(logServer, func(ctx context.Context, ev UserLogin) error {
		logRecorded.Add(1)
		return nil
	})
	eventbus.Subscribe(logServer, func(ctx context.Context, ev OrderFinished) error {
		orderNotified.Add(1)
		return nil
	})

	// apiServer 本地审计（SubscribeLocal 不收远程）
	eventbus.SubscribeLocal(apiServer, func(ctx context.Context, ev UserLogin) error {
		localAudit.Add(1)
		return nil
	})

	// 模拟业务：apiServer 接到 3 次登录
	for i := 0; i < 3; i++ {
		_ = eventbus.Publish(context.Background(), apiServer, UserLogin{
			Uid:       int64(1000 + i),
			IP:        fmt.Sprintf("10.0.0.%d", i),
			Timestamp: time.Now().Unix(),
		})
	}

	// apiServer 产生一笔订单
	_ = eventbus.Publish(context.Background(), apiServer, OrderFinished{
		OrderID: 9001, Uid: 1000, Amount: 100,
	})

	// 验证
	if gameNotified.Load() != 3 {
		t.Fatalf("gameServer expected 3 login notifications, got %d", gameNotified.Load())
	}
	if logRecorded.Load() != 3 {
		t.Fatalf("logServer expected 3 login records, got %d", logRecorded.Load())
	}
	if localAudit.Load() != 3 {
		t.Fatalf("apiServer local audit expected 3, got %d", localAudit.Load())
	}
	if orderNotified.Load() != 1 {
		t.Fatalf("logServer expected 1 order notification, got %d", orderNotified.Load())
	}
}

// TestScenario_HighLoadMixedWorkload 混合高负载场景
func TestScenario_HighLoadMixedWorkload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	mt := eventbus.NewMemoryTransport()
	bus1 := eventbus.New(eventbus.WithTransport(mt))
	bus2 := eventbus.New(eventbus.WithTransport(mt))

	var localSync, localAsync, remoteSync atomic.Int64

	eventbus.Subscribe(bus1, func(ctx context.Context, ev UserLogin) error {
		localSync.Add(1)
		return nil
	})
	eventbus.SubscribeLocal(bus1, func(ctx context.Context, ev OrderFinished) error {
		localAsync.Add(1)
		return nil
	})
	eventbus.Subscribe(bus2, func(ctx context.Context, ev UserLogin) error {
		remoteSync.Add(1)
		return nil
	})

	const n = 5000
	var wg sync.WaitGroup

	// 同步发布 UserLogin
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(uid int64) {
			defer wg.Done()
			_ = eventbus.Publish(context.Background(), bus1, UserLogin{Uid: uid})
		}(int64(i))
	}

	// 异步发布 OrderFinished（PublishLocal + Async 混合）
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(oid int64) {
			defer wg.Done()
			if rand.Intn(2) == 0 {
				_ = eventbus.PublishLocal(context.Background(), bus1, OrderFinished{OrderID: oid})
			} else {
				eventbus.PublishAsync(context.Background(), bus1, OrderFinished{OrderID: oid})
			}
		}(int64(i))
	}

	wg.Wait()
	// 等待异步完成
	time.Sleep(200 * time.Millisecond)

	if localSync.Load() != n {
		t.Fatalf("localSync expected %d, got %d", n, localSync.Load())
	}
	if remoteSync.Load() != n {
		t.Fatalf("remoteSync expected %d, got %d", n, remoteSync.Load())
	}
	if localAsync.Load() != n {
		t.Fatalf("localAsync expected %d, got %d", n, localAsync.Load())
	}
}
