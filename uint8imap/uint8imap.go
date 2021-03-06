package uint8imap

import (
	"encoding/json"
	"strconv"
	"sync"
)

type ConcurrentHashMap struct {
	Shards  int
	HashMap ConcurrentMap
}

// A "thread" safe map of type uint8:Anything.
// To avoid lock bottlenecks this map is dived to several (Shards) map shards.
type ConcurrentMap []*ConcurrentMapShared

// A "thread" safe uint8 to anything map.
type ConcurrentMapShared struct {
	items        map[uint8]interface{}
	sync.RWMutex // Read Write mutex, guards access to internal map.
}

// Creates a new concurrent map.
func New(shards int) *ConcurrentHashMap {
	m := &ConcurrentHashMap{Shards: shards, HashMap: make(ConcurrentMap, shards)}
	for i := 0; i < shards; i++ {
		m.HashMap[i] = &ConcurrentMapShared{items: make(map[uint8]interface{})}
	}
	return m
}

// Returns shard under given key
func (m *ConcurrentHashMap) GetShard(key uint8) *ConcurrentMapShared {
	return m.HashMap[uint32(fnv32(strconv.Itoa(int(key))))%uint32(m.Shards)]
}

// Sets the given map
func (m *ConcurrentHashMap) MSet(data map[uint8]interface{}) {
	for key, value := range data {
		shard := m.GetShard(key)
		shard.Lock()
		shard.items[key] = value
		shard.Unlock()
	}
}

// Sets the given value under the specified key.
func (m *ConcurrentHashMap) Set(key uint8, value interface{}) {
	// Get map shard.
	shard := m.GetShard(key)
	shard.Lock()
	shard.items[key] = value
	shard.Unlock()
}

// Callback to return new element to be inserted into the map
// It is called while lock is held, therefore it MUST NOT
// try to access other keys in same map, as it can lead to deadlock since
// Go sync.RWLock is not reentrant
type UpsertCb func(exist bool, valueInMap interface{}, newValue interface{}) interface{}

// Insert or Update - updates existing element or inserts a new one using UpsertCb
func (m *ConcurrentHashMap) Upsert(key uint8, value interface{}, cb UpsertCb) (res interface{}) {
	shard := m.GetShard(key)
	shard.Lock()
	v, ok := shard.items[key]
	res = cb(ok, v, value)
	shard.items[key] = res
	shard.Unlock()
	return res
}

// Sets the given value under the specified key if no value was associated with it.
func (m *ConcurrentHashMap) SetIfAbsent(key uint8, value interface{}) bool {
	// Get map shard.
	shard := m.GetShard(key)
	shard.Lock()
	_, ok := shard.items[key]
	if !ok {
		shard.items[key] = value
	}
	shard.Unlock()
	return !ok
}

// Retrieves an element from map under given key.
func (m *ConcurrentHashMap) Get(key uint8) (interface{}, bool) {
	// Get shard
	shard := m.GetShard(key)
	shard.RLock()
	// Get item from shard.
	val, ok := shard.items[key]
	shard.RUnlock()
	return val, ok
}

// Returns the number of elements within the map.
func (m *ConcurrentHashMap) Count() int {
	count := 0
	for i := 0; i < m.Shards; i++ {
		shard := m.HashMap[i]
		shard.RLock()
		count += len(shard.items)
		shard.RUnlock()
	}
	return count
}

// Looks up an item under specified key
func (m *ConcurrentHashMap) Has(key uint8) bool {
	// Get shard
	shard := m.GetShard(key)
	shard.RLock()
	// See if element is within shard.
	_, ok := shard.items[key]
	shard.RUnlock()
	return ok
}

// Removes an element from the map.
func (m *ConcurrentHashMap) Remove(key uint8) {
	// Try to get shard.
	shard := m.GetShard(key)
	shard.Lock()
	delete(shard.items, key)
	shard.Unlock()
}

// Removes an element from the map and returns it
func (m *ConcurrentHashMap) Pop(key uint8) (v interface{}, exists bool) {
	// Try to get shard.
	shard := m.GetShard(key)
	shard.Lock()
	v, exists = shard.items[key]
	delete(shard.items, key)
	shard.Unlock()
	return v, exists
}

// Checks if map is empty.
func (m *ConcurrentHashMap) IsEmpty() bool {
	return m.Count() == 0
}

// Used by the Iter & IterBuffered functions to wrap two variables together over a channel,
type Tuple struct {
	Key uint8
	Val interface{}
}

// Returns an iterator which could be used in a for range loop.
//
// Deprecated: using IterBuffered() will get a better performence
func (m *ConcurrentHashMap) Iter() <-chan Tuple {
	chans := snapshot(m)
	ch := make(chan Tuple)
	go fanIn(chans, ch)
	return ch
}

// Returns a buffered iterator which could be used in a for range loop.
func (m *ConcurrentHashMap) IterBuffered() <-chan Tuple {
	chans := snapshot(m)
	total := 0
	for _, c := range chans {
		total += cap(c)
	}
	ch := make(chan Tuple, total)
	go fanIn(chans, ch)
	return ch
}

// Returns a array of channels that contains elements in each shard,
// which likely takes a snapshot of `m`.
// It returns once the size of each buffered channel is determined,
// before all the channels are populated using goroutines.
func snapshot(m *ConcurrentHashMap) (chans []chan Tuple) {
	chans = make([]chan Tuple, m.Shards)
	wg := sync.WaitGroup{}
	wg.Add(m.Shards)
	// Foreach shard.
	for index, shard := range m.HashMap {
		go func(index int, shard *ConcurrentMapShared) {
			// Foreach key, value pair.
			shard.RLock()
			chans[index] = make(chan Tuple, len(shard.items))
			wg.Done()
			for key, val := range shard.items {
				chans[index] <- Tuple{key, val}
			}
			shard.RUnlock()
			close(chans[index])
		}(index, shard)
	}
	wg.Wait()
	return chans
}

// fanIn reads elements from channels `chans` into channel `out`
func fanIn(chans []chan Tuple, out chan Tuple) {
	wg := sync.WaitGroup{}
	wg.Add(len(chans))
	for _, ch := range chans {
		go func(ch chan Tuple) {
			for t := range ch {
				out <- t
			}
			wg.Done()
		}(ch)
	}
	wg.Wait()
	close(out)
}

// Returns all items as map[uint8]interface{}
func (m *ConcurrentHashMap) Items() map[uint8]interface{} {
	tmp := make(map[uint8]interface{})

	// Insert items to temporary map.
	for item := range m.IterBuffered() {
		tmp[item.Key] = item.Val
	}

	return tmp
}

// Iterator callback,called for every key,value found in
// maps. RLock is held for all calls for a given shard
// therefore callback sess consistent view of a shard,
// but not across the shards
type IterCb func(key uint8, v interface{})

// Callback based iterator, cheapest way to read
// all elements in a map.
func (m *ConcurrentHashMap) IterCb(fn IterCb) {
	for idx := range m.HashMap {
		shard := m.HashMap[idx]
		shard.RLock()
		for key, value := range shard.items {
			fn(key, value)
		}
		shard.RUnlock()
	}
}

// Return all keys as []uint8
func (m *ConcurrentHashMap) Keys() []uint8 {
	count := m.Count()
	ch := make(chan uint8, count)
	go func() {
		// Foreach shard.
		wg := sync.WaitGroup{}
		wg.Add(m.Shards)
		for _, shard := range m.HashMap {
			go func(shard *ConcurrentMapShared) {
				// Foreach key, value pair.
				shard.RLock()
				for key := range shard.items {
					ch <- key
				}
				shard.RUnlock()
				wg.Done()
			}(shard)
		}
		wg.Wait()
		close(ch)
	}()

	// Generate keys
	keys := make([]uint8, 0, count)
	for k := range ch {
		keys = append(keys, k)
	}
	return keys
}

//Reviles ConcurrentHashMap "private" variables to json marshal.
func (m *ConcurrentHashMap) MarshalJSON() ([]byte, error) {
	// Create a temporary map, which will hold all item spread across shards.
	tmp := make(map[uint8]interface{})

	// Insert items to temporary map.
	for item := range m.IterBuffered() {
		tmp[item.Key] = item.Val
	}
	return json.Marshal(tmp)
}

func fnv32(key string) uint32 {
	hash := uint32(2166136261)
	const prime32 = uint32(16777619)
	for i := 0; i < len(key); i++ {
		hash *= prime32
		hash ^= uint32(key[i])
	}
	return hash
}

// Sets the given value under the specified key if oldValue was associated with it.
func (m *ConcurrentHashMap) SetIfPresent(key uint8, newValue, oldValue interface{}) bool {
	// Get map shard.
	shard := m.GetShard(key)
	shard.Lock()
	val, ok := shard.items[key]
	ok = ok && (val == oldValue)
	if ok {
		shard.items[key] = newValue
	}
	shard.Unlock()
	return ok
}

// Sets the given value under the specified key if oldValue was associated with it.
func (m *ConcurrentHashMap) AddIfPresent(key uint8, value interface{}) bool {
	// Get map shard.
	shard := m.GetShard(key)
	shard.Lock()
	val, ok := shard.items[key]
	if ok {
		tmp := val.([]interface{})
		tmp = append(tmp, value)
		shard.items[key] = tmp
	} else {
		shard.items[key] = value
	}
	shard.Unlock()
	return ok
}

type fun func(key uint8, oldVal, newVal interface{}) interface{}

func (m *ConcurrentHashMap) AddIfPresentWithFun(key uint8, newVal interface{}, fn fun) {
	// Get map shard.
	shard := m.GetShard(key)
	shard.Lock()
	oldVal, ok := shard.items[key]
	if ok {
		shard.items[key] = fn(key, oldVal, newVal)
	} else {
		shard.items[key] = newVal
	}
	shard.Unlock()
}

// Removes an element from the map.
func (m *ConcurrentHashMap) Erase() {
	for k, _ := range m.Items() {
		// Try to get shard.
		shard := m.GetShard(k)
		shard.Lock()
		delete(shard.items, k)
		shard.Unlock()
	}
}
