package covers

import (
	"container/list"
	"context"
	"sync"
)

// Backend is what Cached wraps: Disk or Dir.
type Backend interface {
	Put(ctx context.Context, name string, data []byte) error
	Get(ctx context.Context, name string) ([]byte, error)
	Delete(ctx context.Context, name string) error
}

// Cached keeps recently shown covers in memory, up to maxBytes in total, least recently used
// out first. Without it every cover shown costs two requests to Yandex Disk. Safe because a cover
// file never changes once written.
type Cached struct {
	Backend
	maxBytes int

	mu    sync.Mutex
	size  int
	order *list.List               // front = most recently used; values are *entry
	items map[string]*list.Element // name -> element in order
}

type entry struct {
	name string
	data []byte
}

func NewCached(b Backend, maxBytes int) *Cached {
	return &Cached{Backend: b, maxBytes: maxBytes, order: list.New(), items: make(map[string]*list.Element)}
}

func (c *Cached) Get(ctx context.Context, name string) ([]byte, error) {
	if data, ok := c.lookup(name); ok {
		return data, nil
	}
	data, err := c.Backend.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	c.add(name, data)
	return data, nil
}

// Put also caches the new cover: the member who uploaded it is about to see it.
func (c *Cached) Put(ctx context.Context, name string, data []byte) error {
	if err := c.Backend.Put(ctx, name, data); err != nil {
		return err
	}
	c.add(name, data)
	return nil
}

// Publish passes through to the backend when it can publish (Disk), else ErrCannotPublish.
func (c *Cached) Publish(ctx context.Context, name string) (string, error) {
	if p, ok := c.Backend.(Publisher); ok {
		return p.Publish(ctx, name)
	}
	return "", ErrCannotPublish
}

func (c *Cached) Delete(ctx context.Context, name string) error {
	c.mu.Lock()
	if el, ok := c.items[name]; ok {
		c.remove(el)
	}
	c.mu.Unlock()
	return c.Backend.Delete(ctx, name)
}

func (c *Cached) lookup(name string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[name]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*entry).data, true
}

func (c *Cached) add(name string, data []byte) {
	if len(data) > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[name]; ok {
		c.remove(el)
	}
	c.items[name] = c.order.PushFront(&entry{name: name, data: data})
	c.size += len(data)
	for c.size > c.maxBytes {
		c.remove(c.order.Back())
	}
}

// remove: caller holds c.mu.
func (c *Cached) remove(el *list.Element) {
	e := el.Value.(*entry)
	c.order.Remove(el)
	delete(c.items, e.name)
	c.size -= len(e.data)
}
