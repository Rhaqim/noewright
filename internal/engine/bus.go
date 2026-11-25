package engine

import "sync"

// bus.go provides the per-turn cross-agent communication fabric. Agents in a turn
// publish and subscribe to named topics over channels; RunTurn creates one Bus
// per turn and hands it to every agent through GenerateRequest.Bus. Because the
// lead runs before the followers, the Bus keeps a replay log: a follower that
// subscribes after the lead has already published still receives the backlog (in
// order) and then any further live messages, until the turn ends and the Bus is
// closed (every subscriber channel is closed, so `range` consumers terminate).
//
// Use it for live coordination the staged Inputs hand-off can't express — e.g. a
// follower consuming the lead's tokens as they stream, or two concurrent
// followers exchanging structured cues mid-flight.
//
// A single dispatcher goroutine owns every subscriber channel, so publishes,
// subscribes, and close never race. Subscriber channels are buffered for turn
// scale.

// Message is one item published on a turn topic.
type Message struct {
	From  string // agent slug that published it ("" for engine-published)
	Topic string
	Data  any
	Seq   int // monotonic per-bus sequence number
}

// Bus is the cross-agent channel fabric for a single turn.
type Bus interface {
	// Publish sends data on a topic to all current and future subscribers.
	Publish(from, topic string, data any)
	// Subscribe returns a channel delivering every message on topic — first the
	// replay backlog (in order), then live messages — until the turn ends, when
	// the channel is closed. Pass "*" (TopicAll) to receive every topic.
	Subscribe(topic string) <-chan Message
	// Messages returns a snapshot of everything published so far.
	Messages() []Message
}

// TopicAll subscribes to every topic.
const TopicAll = "*"

const busChanBuffer = 256

type busMsg struct {
	from, topic string
	data        any
	// flushed, when set, marks a flush request rather than a message: the
	// dispatcher closes it once every message enqueued before it is logged.
	flushed chan struct{}
}

type busImpl struct {
	in   chan busMsg
	sub  chan subscription
	done chan struct{}

	mu  sync.Mutex
	log []Message
}

type subscription struct {
	topic string
	ch    chan Message
}

func newBus() *busImpl {
	b := &busImpl{
		in:   make(chan busMsg, busChanBuffer),
		sub:  make(chan subscription),
		done: make(chan struct{}),
	}
	go b.run()
	return b
}

// deliver sends one message to a subscriber without ever blocking the sole
// dispatcher: it honours turn-end (b.done) and drops the message to a subscriber
// whose 256-slot buffer is already full. Dropping a straggler is preferable to
// wedging the dispatcher, which would otherwise block every publisher for the
// rest of the turn.
func (b *busImpl) deliver(s subscription, msg Message) {
	select {
	case s.ch <- msg:
	case <-b.done:
	default:
	}
}

// run is the sole owner of subscriber channels.
func (b *busImpl) run() {
	var subs []subscription
	var seq int
	for {
		select {
		case m := <-b.in:
			if m.flushed != nil {
				close(m.flushed)
				continue
			}
			seq++
			msg := Message{From: m.from, Topic: m.topic, Data: m.data, Seq: seq}
			b.mu.Lock()
			b.log = append(b.log, msg)
			b.mu.Unlock()
			for _, s := range subs {
				if s.topic == TopicAll || s.topic == msg.Topic {
					b.deliver(s, msg)
				}
			}
		case s := <-b.sub:
			b.mu.Lock()
			backlog := append([]Message(nil), b.log...)
			b.mu.Unlock()
			for _, msg := range backlog {
				if s.topic == TopicAll || s.topic == msg.Topic {
					b.deliver(s, msg)
				}
			}
			subs = append(subs, s)
		case <-b.done:
			for _, s := range subs {
				close(s.ch)
			}
			return
		}
	}
}

func (b *busImpl) Publish(from, topic string, data any) {
	select {
	case b.in <- busMsg{from: from, topic: topic, data: data}:
	case <-b.done:
	}
}

func (b *busImpl) Subscribe(topic string) <-chan Message {
	ch := make(chan Message, busChanBuffer)
	select {
	case b.sub <- subscription{topic: topic, ch: ch}:
	case <-b.done:
		close(ch)
	}
	return ch
}

// Messages returns everything published so far. Publish only enqueues, so it
// first waits for the dispatcher to log every message published before the
// call; without that, a snapshot taken right after the last Publish (as a turn
// does when it ends) could miss the newest messages.
func (b *busImpl) Messages() []Message {
	b.flush()
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Message(nil), b.log...)
}

// flush waits until the dispatcher has logged every message enqueued before
// it. It returns at once if the bus is closed.
func (b *busImpl) flush() {
	done := make(chan struct{})
	select {
	case b.in <- busMsg{flushed: done}:
	case <-b.done:
		return
	}
	select {
	case <-done:
	case <-b.done:
	}
}

// close ends the turn; the dispatcher closes every subscriber channel. Safe to
// call once.
func (b *busImpl) close() {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
}
