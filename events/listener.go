package events

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/sahib/brig/events/backend"
	"github.com/sahib/config"
)

const (
	brigEventTopicPrefix = "brig/events/"
)

// Listener listens to incoming events from other remotes.
// For every event, a registered callback can be executed.
// It does not implement net.Listener and is only similar from a concept POV.
type Listener struct {
	mu sync.Mutex

	bk        backend.Backend
	cfg       *config.Config
	callbacks map[EventType]func(*Event)
	cancels   map[string]context.CancelFunc
	evSendCh  chan Event
	evRecvCh  chan Event
	ownAddr   string
	isClosed  bool
}

// NewListener constructs a new listener.
// `cfg` is used to read the event subsystem config.
// `bk` is a events.Backend.
// `ownAddr` is the addr of our own node.
func NewListener(cfg *config.Config, bk backend.Backend, ownAddr string) *Listener {
	lst := &Listener{
		bk:        bk,
		cfg:       cfg,
		ownAddr:   ownAddr,
		callbacks: make(map[EventType]func(*Event)),
		cancels:   make(map[string]context.CancelFunc),
		evSendCh:  make(chan Event, 100),
		evRecvCh:  make(chan Event, 100),
	}

	go lst.eventSendLoop()
	go lst.eventRecvLoop()
	return lst
}

// Close will close all open listeners and clean up internal resources.
func (lst *Listener) Close() error {
	lst.mu.Lock()
	defer lst.mu.Unlock()

	if lst.isClosed {
		return nil
	}

	close(lst.evSendCh)
	close(lst.evRecvCh)

	for _, cancel := range lst.cancels {
		cancel()
	}

	lst.isClosed = true
	return nil
}

// RegisterEventHandler remembers that `hdl` should be called whenever a event
// of type `ev` is being received.
func (lst *Listener) RegisterEventHandler(ev EventType, hdl func(ev *Event)) {
	lst.mu.Lock()
	defer lst.mu.Unlock()

	if lst.isClosed {
		return
	}

	lst.callbacks[ev] = hdl
}

func (lst *Listener) eventSendLoop() {
	events := []Event{}
	tckr := time.NewTicker(lst.cfg.Duration("send_flush_window"))
	defer tckr.Stop()

	ownTopic := brigEventTopicPrefix + lst.ownAddr

	for {
		select {
		case <-tckr.C:
			for _, ev := range dedupeEvents(events) {
				data, err := ev.encode()
				if err != nil {
					log.Errorf("event: failed to encode: %v", err)
					continue
				}

				if err := lst.bk.PublishEvent(ownTopic, data); err != nil {
					log.Errorf("event: failed to publish: %v", err)
					continue
				}
			}

			events = []Event{}
		case ev, ok := <-lst.evSendCh:
			if !ok {
				return
			}

			events = append(events, ev)
		}
	}
}

func (lst *Listener) eventRecvLoop() {
	events := []Event{}
	tckr := time.NewTicker(lst.cfg.Duration("recv_flush_window"))
	defer tckr.Stop()

	for {
		select {
		case <-tckr.C:
			for _, ev := range dedupeEvents(events) {
				lst.mu.Lock()
				if cb, ok := lst.callbacks[ev.Type]; ok {
					go cb(&ev)
				}
				lst.mu.Unlock()
			}

			events = []Event{}
		case ev, ok := <-lst.evRecvCh:
			if !ok {
				return
			}

			events = append(events, ev)
		}
	}
}

// PublishEvent notifies other peers that something on our
// side changed. The "something" is defined by `ev`.
// PublishEvent does not block.
func (lst *Listener) PublishEvent(ev Event) error {
	lst.mu.Lock()
	defer lst.mu.Unlock()

	if lst.isClosed {
		return nil
	}

	if !lst.cfg.Bool("enabled") {
		return nil
	}

	// Only send the event if we are not clogged up yet.
	// We prioritze the well-being of other systems more by
	// not allowing PublishEvent to block.
	select {
	case lst.evSendCh <- ev:
		return nil
	default:
		return fmt.Errorf("lost event")
	}
}

// SetupListeners sets up the listener to receive events from any of `addrs`.
// If `ctx` is being canceled, all listeners will stop.
// SetupListeners can be called several times, each time overwriting and stopping
// previous listeners.
func (lst *Listener) SetupListeners(ctx context.Context, addrs []string) error {
	if lst.isClosed {
		return nil
	}

	seen := make(map[string]bool)

	for _, addr := range addrs {
		seen[addr] = true
		cancel, ok := lst.cancels[addr]
		if ok {
			// We already have a listener for this.
			continue
		}

		ctx, cancel := context.WithCancel(ctx)
		lst.cancels[addr] = cancel
		go lst.listenSingle(ctx, brigEventTopicPrefix+addr)
	}

	// cancel all listeners that are not needed anymore.
	for addr, cancel := range lst.cancels {
		if !seen[addr] {
			cancel()
		}
	}

	return nil
}

func (lst *Listener) listenSingle(ctx context.Context, topic string) error {
	sub, err := lst.bk.Subscribe(ctx, topic)
	if err != nil {
		return err
	}

	defer sub.Close()

	log.Debugf("listening for events on %s", topic)
	defer log.Debugf("event listener on %s closing", topic)

	for {
		if !lst.cfg.Bool("enabled") {
			continue
		}

		msg, err := sub.Next(ctx)
		if msg == nil {
			continue
		}

		if err == io.EOF || err == context.Canceled {
			return nil
		} else if err != nil {
			return err
		}

		if msg.Source() == lst.ownAddr {
			continue
		}

		ev, err := decodeMessage(msg.Data())
		if err != nil {
			log.Warningf("received bad message: %v", err)
			continue
		}

		ev.Source = msg.Source()

		select {
		case lst.evRecvCh <- *ev:
		default:
			log.Warningf("dropped incoming event: %v", ev)
		}
	}

	return nil
}
