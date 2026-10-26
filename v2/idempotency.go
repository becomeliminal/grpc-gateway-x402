package x402

import (
	"sync"
	"time"
)

// ReceiptRecord is a settled response remembered for a payment identifier.
// A retry with the same id returns this record and does not call /settle again.
type ReceiptRecord struct {
	Payment     PaymentResponse
	StatusCode  int
	Body        []byte
	Header      map[string][]string
	GRPCResp    interface{}
	HasGRPC     bool
	Fingerprint string
}

// ReceiptStore is a bounded TTL cache of settled receipts.
type ReceiptStore interface {
	Lookup(id string) (ReceiptRecord, bool)
	Store(id string, rec ReceiptRecord, ttl time.Duration) error
}

type memoryItem struct {
	rec     ReceiptRecord
	expires time.Time
}

// MemoryReceiptStore is a process-local receipt cache.
type MemoryReceiptStore struct {
	mu    sync.Mutex
	max   int
	items map[string]memoryItem
	now   func() time.Time
}

// NewMemoryReceiptStore returns an in-memory store that keeps at most max entries.
// A non-positive max selects 1024.
func NewMemoryReceiptStore(max int) *MemoryReceiptStore {
	if max <= 0 {
		max = 1024
	}
	return &MemoryReceiptStore{
		max:   max,
		items: make(map[string]memoryItem),
		now:   time.Now,
	}
}

// Lookup returns a receipt that has not expired.
func (s *MemoryReceiptStore) Lookup(id string) (ReceiptRecord, bool) {
	if id == "" {
		return ReceiptRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	if !ok {
		return ReceiptRecord{}, false
	}
	if !item.expires.After(s.now()) {
		delete(s.items, id)
		return ReceiptRecord{}, false
	}
	return item.rec, true
}

// Store saves a receipt until ttl elapses. A zero ttl keeps it for 10 minutes.
func (s *MemoryReceiptStore) Store(id string, rec ReceiptRecord, ttl time.Duration) error {
	if id == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for key, item := range s.items {
		if !item.expires.After(now) {
			delete(s.items, key)
		}
	}
	if _, exists := s.items[id]; !exists && len(s.items) >= s.max {
		s.evictOneLocked()
	}
	rec.Body = append([]byte(nil), rec.Body...)
	rec.Header = cloneHeader(rec.Header)
	s.items[id] = memoryItem{rec: rec, expires: now.Add(ttl)}
	return nil
}

func cloneHeader(h map[string][]string) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string][]string, len(h))
	for key, values := range h {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func (s *MemoryReceiptStore) evictOneLocked() {
	var oldestKey string
	var oldest time.Time
	for key, item := range s.items {
		if oldestKey == "" || item.expires.Before(oldest) {
			oldestKey = key
			oldest = item.expires
		}
	}
	if oldestKey != "" {
		delete(s.items, oldestKey)
	}
}
