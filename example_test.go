package eventbus_test

import (
	"context"
	"fmt"

	"github.com/vincentAlen/eventbus"
)

type ChatMessage struct {
	From string `json:"from"`
	Text string `json:"text"`
}

func (ChatMessage) Topic() string { return "chat.message" }

func ExampleNew() {
	bus := eventbus.New()
	fmt.Println("bus ID is non-empty:", bus.ID() != "")
	// Output:
	// bus ID is non-empty: true
}

func ExampleSubscribe() {
	bus := eventbus.New()

	eventbus.Subscribe[ChatMessage](bus, func(ctx context.Context, ev ChatMessage) error {
		fmt.Printf("%s says: %s\n", ev.From, ev.Text)
		return nil
	})

	_ = eventbus.Publish[ChatMessage](context.Background(), bus, ChatMessage{
		From: "Alice",
		Text: "Hello!",
	})
	// Output:
	// Alice says: Hello!
}

func ExampleNew_withTransport() {
	mt := eventbus.NewMemoryTransport()
	bus1 := eventbus.New(eventbus.WithTransport(mt))
	bus2 := eventbus.New(eventbus.WithTransport(mt))

	eventbus.Subscribe[ChatMessage](bus2, func(ctx context.Context, ev ChatMessage) error {
		fmt.Printf("bus2 received: %s from %s\n", ev.Text, ev.From)
		return nil
	})

	_ = eventbus.Publish[ChatMessage](context.Background(), bus1, ChatMessage{
		From: "Bob",
		Text: "Hi from bus1!",
	})
	// Output:
	// bus2 received: Hi from bus1! from Bob

	_ = bus1.Close()
	_ = bus2.Close()
}
