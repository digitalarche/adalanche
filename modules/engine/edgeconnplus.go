package engine

import (
	"sort"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/jfcg/sorty/v2"
	"github.com/lkarlslund/adalanche/modules/ui"
)

type EdgeConnectionsPlus struct {
	backing unsafe.Pointer
	mu      sync.RWMutex
	lastOp  atomic.Uint32
	growing atomic.Uint32
}

type Connections struct {
	data     []Connection
	maxClean uint32        // Not atomic, this is always only being read (number of sorted items)
	maxTotal atomic.Uint32 // Number of total items
	deleted  atomic.Uint32 // Number of deleted items
}

type Connection struct {
	target *Object
	alive  uint32
	edges  EdgeBitmap
}

func (e *EdgeConnectionsPlus) init() {

}

func (e *EdgeConnectionsPlus) Range(af func(key *Object, value EdgeBitmap) bool) {
	backing := e.getBacking()
	if backing == nil {
		return
	}
	last := int(backing.maxTotal.Load())
	if last == 0 {
		return
	}

	data := backing.data[:last]
	for i := range data {
		if atomic.LoadUint32(&data[i].alive) == 1 {
			if !af(data[i].target, data[i].edges) {
				break
			}
		}
	}
}

func (e *EdgeConnectionsPlus) getBacking() *Connections {
	return (*Connections)(atomic.LoadPointer(&e.backing))
}

func (e *EdgeConnectionsPlus) search(wantedKey *Object) *Connection {
	backing := e.getBacking()
	if backing != nil {
		uintWantedKey := uintptr(unsafe.Pointer(wantedKey))
		n, found := sort.Find(int(backing.maxClean), func(i int) int {
			foundKey := uintptr(unsafe.Pointer(backing.data[i].target))
			return int(uintWantedKey - foundKey)
		})
		if found {
			return &backing.data[n]
		}
		max := backing.maxTotal.Load()
		for i := backing.maxClean; i < max; i++ {
			foundKey := backing.data[i].target
			if foundKey == wantedKey {
				return &backing.data[i]
			}
		}
	}
	return nil
}

func (e *EdgeConnectionsPlus) del(target *Object) {
	e.rlock()
	conn := e.search(target)
	if conn != nil {
		if conn.target != target {
			panic("WRONG")
		}
		if atomic.CompareAndSwapUint32(&conn.alive, 1, 0) {
			backing := e.getBacking()
			backing.deleted.Add(1)

			// if backing.deleted.Load() > backing.maxTotal.Load()/2 {
			// 	e.runlock()
			// 	e.lock()
			// 	e.resize()
			// 	e.unlock()
			// 	return
			// }
		} else {
			ui.Debug().Msg("Trying to delete edge that is already deleted")
		}
	} else {
		ui.Debug().Msg("Trying to delete edge that could not be found")
	}
	e.runlock()
}

func (e *EdgeConnectionsPlus) Len() int {
	backing := e.getBacking()
	if backing == nil {
		return 0
	}
	return int(backing.maxTotal.Load() - backing.deleted.Load())
}

func (e *EdgeConnectionsPlus) PreciseLen() int {
	backing := e.getBacking()
	if backing == nil {
		return 0
	}
	var length int
	max := int(backing.maxTotal.Load())
	for i := 0; i < max && i < len(backing.data); /* BCE */ i++ {
		if atomic.LoadUint32(&backing.data[i].alive) == 1 {
			length++
		}
	}
	return length
}

func (e *EdgeConnectionsPlus) setEdge(target *Object, edge Edge) {
	e.modifyEdges(target, func(oldEdges *EdgeBitmap) {
		oldEdges.AtomicSet(edge)
	}, true, false)
}

func (e *EdgeConnectionsPlus) clearEdge(target *Object, edge Edge) {
	e.modifyEdges(target, func(oldEdges *EdgeBitmap) {
		oldEdges.AtomicClear(edge)
	}, false, true)
}

func (e *EdgeConnectionsPlus) setEdges(target *Object, edges EdgeBitmap) {
	e.modifyEdges(target, func(oldEdges *EdgeBitmap) {
		oldEdges.AtomicOr(edges)
	}, true, false)
}

func (e *EdgeConnectionsPlus) modifyEdges(target *Object, mf func(edges *EdgeBitmap), insertIfNotFound, deleteIfBlank bool) {
	e.rlock()
	connection := e.search(target)
	if connection != nil {
		mf(&connection.edges)
		if deleteIfBlank && connection.edges.IsBlank() {
			if atomic.CompareAndSwapUint32(&connection.alive, 1, 0) {
				backing := e.getBacking()
				backing.deleted.Add(1)
			}
		} else {
			if atomic.CompareAndSwapUint32(&connection.alive, 0, 1) {
				backing := e.getBacking()
				backing.deleted.Add(^uint32(0))
			}
		}
		e.runlock()
		return
	}
	// Not found
	if !insertIfNotFound {
		// Not asked to insert it
		return
	}

	oldBacking := e.getBacking()
	var oldMax uint32
	if oldBacking != nil {
		oldMax = oldBacking.maxTotal.Load()
	}

	e.runlock()

	e.lock()

	// There was someone else doing changes, maybe they inserted it?
	newBacking := e.getBacking()
	if oldBacking == newBacking && newBacking != nil {
		// Only a few was inserted, so just search those
		newMax := newBacking.maxTotal.Load()
		for i := oldMax; i < newMax; i++ {
			if newBacking.data[i].target == target {
				connection = &newBacking.data[i]
			}
		}
	} else {
		// the backing was switched, so search again
		connection = e.search(target)
	}

	if connection != nil {
		mf(&connection.edges)
		if deleteIfBlank && connection.edges.IsBlank() {
			if atomic.CompareAndSwapUint32(&connection.alive, 1, 0) {
				backing := e.getBacking()
				backing.deleted.Add(1)
			}
		} else {
			if atomic.CompareAndSwapUint32(&connection.alive, 0, 1) {
				backing := e.getBacking()
				backing.deleted.Add(^uint32(0))
			}
		}
		e.unlock()
		return
	}

	var newedges EdgeBitmap
	mf(&newedges)
	e.insert(target, newedges)

	e.unlock()
}

func (e *EdgeConnectionsPlus) insert(target *Object, eb EdgeBitmap) {
	newConnection := Connection{
		target: target,
		edges:  eb,
		alive:  1,
	}

	backing := e.getBacking()

	for backing == nil || int(backing.maxTotal.Load()) == len(backing.data) {
		e.maintainBacking(false)
		backing = e.getBacking()
	}

	newMax := backing.maxTotal.Add(1)
	backing.data[int(newMax-1)] = newConnection
}

func (e *EdgeConnectionsPlus) Optimize() {
	e.lock()
	e.maintainBacking(true)
	e.unlock()
}

func (e *EdgeConnectionsPlus) maintainBacking(keepsize bool) {
	if !e.growing.CompareAndSwap(0, 1) {
		panic("growing twice")
	}
	oldBacking := e.getBacking()
	if oldBacking == nil {
		// first time we're getting dirty around there
		newBacking := Connections{
			data: make([]Connection, 4),
		}
		atomic.StorePointer(&e.backing, unsafe.Pointer(&newBacking))
		e.growing.Store(0)
		return
	}

	oldMax := int(oldBacking.maxTotal.Load())
	var addLength int
	if !keepsize {
		addLength = len(oldBacking.data)
		if addLength > 8192 {
			addLength = 8192
		}
	}
	newLength := len(oldBacking.data) + addLength

	// Don't add, just keep this size
	if oldBacking.deleted.Load() > uint32(oldMax)/4 {
		addLength = 0
	}

	newData := make([]Connection, newLength)
	copy(newData, oldBacking.data)

	var deleted uint32
	searchData := newData[:oldMax]
	for i := range searchData { // BCE
		if atomic.LoadUint32(&searchData[i].alive) == 0 {
			searchData[i].target = (*Object)(unsafe.Pointer(^uintptr(0))) // Max pointer, sort will move it to the end
			deleted++
		}
	}

	// sort.Sort(ConnectionSliceSorter(newData[:oldMax]))
	sorty.MaxGor = 1
	sorty.Sort(oldMax, func(i, k, r, s int) bool {
		if uintptr(unsafe.Pointer(newData[i].target)) < uintptr(unsafe.Pointer(newData[k].target)) {
			if r != s {
				newData[r], newData[s] = newData[s], newData[r]
			}
			return true
		}
		return false
	})
	// sort.Slice(newData[:oldMax], func(i, j int) bool {
	// 	return uintptr(unsafe.Pointer(newData[i].target)) < uintptr(unsafe.Pointer(newData[j].target))
	// })

	// if oldBacking.deleted.Load() > uint32(oldMax)/2 {
	// 	// More than half was deleted, lets shrink to a new slice
	// 	shrinkData := make([]Connection, oldMax/2)
	// 	copy(shrinkData, newData)
	// 	newData = shrinkData
	// }

	newBacking := Connections{
		data:     newData,
		maxClean: uint32(oldMax) - deleted,
	}
	newBacking.maxTotal.Store(newBacking.maxClean)

	if !atomic.CompareAndSwapPointer(&e.backing, unsafe.Pointer(oldBacking), unsafe.Pointer(&newBacking)) {
		panic("Backing was changed behind my back")
	}
	e.growing.Store(0)
}

// Do a read lock
func (e *EdgeConnectionsPlus) rlock() uint32 {
	e.mu.RLock()
	return e.lastOp.Load()
}

func (e *EdgeConnectionsPlus) runlock() {
	e.mu.RUnlock()
}

// Upgrades lock and returns whether there was changes in the mean time
func (e *EdgeConnectionsPlus) upgradelock(lastOp uint32) bool {
	e.mu.RUnlock()
	e.mu.Lock()
	return e.lastOp.Load() == lastOp
}

// Do a write lock
func (e *EdgeConnectionsPlus) lock() {
	e.mu.Lock()
}

func (e *EdgeConnectionsPlus) unlock() {
	e.mu.Unlock()
}

type ConnectionSliceSorter []Connection

func (cs ConnectionSliceSorter) Len() int {
	return len(cs)
}

func (cs ConnectionSliceSorter) Less(i, j int) bool {
	return uintptr(unsafe.Pointer(cs[i].target)) < uintptr(unsafe.Pointer(cs[j].target))
}

func (cs ConnectionSliceSorter) Swap(i, j int) {
	cs[i], cs[j] = cs[j], cs[i]
}