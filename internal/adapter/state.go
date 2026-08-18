package adapter

import (
	"sync"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

type frameStore struct {
	mu         sync.RWMutex
	generation uint64
	frame      *Frame
}

func (s *frameStore) beginGeneration(generation uint64) {
	s.mu.Lock()
	s.generation = generation
	s.frame = nil
	s.mu.Unlock()
}

func (s *frameStore) store(generation, sequence uint64, rgb []byte, receivedAt time.Time) bool {
	if len(rgb) != floor.RGBByteCount {
		return false
	}
	var value Frame
	value.Sequence = sequence
	value.ReceivedAt = receivedAt
	copy(value.RGB[:], rgb)

	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return false
	}
	s.frame = &value
	return true
}

func (s *frameStore) snapshot() (Frame, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.frame == nil {
		return Frame{}, false
	}
	return *s.frame, true
}

// presentationSnapshot holds a read lock until release is called. Engine
// replacement takes the write lock, so once a replacement attach completes no
// transaction can still be sending a frame from the retired generation.
func (s *frameStore) presentationSnapshot() (Frame, bool, func()) {
	s.mu.RLock()
	if s.frame == nil {
		return Frame{}, false, s.mu.RUnlock
	}
	return *s.frame, true, s.mu.RUnlock
}

type pressureChange struct {
	X       int
	Y       int
	Pressed bool
}

const historyMinutes = 60

type TileStats struct {
	Presses            uint64
	PressedDurationSec float64
}

type FloorStatsSnapshot struct {
	ActivePressedTiles uint32
	Tiles              [floor.TileCount]TileStats
}

type pressureStore struct {
	mu         sync.RWMutex
	sequence   uint64
	observedAt time.Time
	bits       [floor.PressureByteCount]byte

	activePressedTiles    uint32
	tilePresses           [floor.TileCount]uint64
	tilePressedDurationNs [floor.TileCount]uint64
	tilePressedSince      [floor.TileCount]int64

	lastBucketMinute int64
	minuteBuckets    [historyMinutes][floor.TileCount]uint32
	minuteDwellNs    [historyMinutes][floor.TileCount]uint32

	subscribers map[chan struct{}]struct{}
}

func (s *pressureStore) advanceBucketsLocked(minute int64) {
	if s.lastBucketMinute == 0 {
		s.lastBucketMinute = minute
		return
	}
	if minute <= s.lastBucketMinute {
		return
	}
	gap := int(minute - s.lastBucketMinute)
	if gap > historyMinutes {
		gap = historyMinutes
	}
	for i := 1; i <= gap; i++ {
		idx := (s.lastBucketMinute + int64(i)) % historyMinutes
		s.minuteBuckets[idx] = [floor.TileCount]uint32{}
		s.minuteDwellNs[idx] = [floor.TileCount]uint32{}
	}
	s.lastBucketMinute = minute
}

func (s *pressureStore) apply(changes []pressureChange, observedAt time.Time) (PressureSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	minute := observedAt.Unix() / 60
	s.advanceBucketsLocked(minute)
	bIdx := minute % historyMinutes

	changed := false
	for _, change := range changes {
		if !floor.InLogicalBounds(change.X, change.Y) {
			continue
		}
		index := change.Y*floor.GridWidth + change.X
		mask := byte(1 << uint(index%8))
		wasPressed := s.bits[index/8]&mask != 0
		if wasPressed == change.Pressed {
			continue
		}
		if change.Pressed {
			s.bits[index/8] |= mask
			s.tilePresses[index]++
			s.minuteBuckets[bIdx][index]++
			s.tilePressedSince[index] = observedAt.UnixNano()
			s.activePressedTiles++
		} else {
			s.bits[index/8] &^= mask
			if start := s.tilePressedSince[index]; start > 0 {
				if observedAt.UnixNano() > start {
					delta := uint64(observedAt.UnixNano() - start)
					s.tilePressedDurationNs[index] += delta
					s.minuteDwellNs[bIdx][index] += uint32(delta)
				}
				s.tilePressedSince[index] = 0
			}
			if s.activePressedTiles > 0 {
				s.activePressedTiles--
			}
		}
		changed = true
	}
	if !changed {
		return s.snapshotLocked(), false
	}
	s.sequence++
	s.observedAt = observedAt

	for ch := range s.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return s.snapshotLocked(), true
}

func (s *pressureStore) snapshot() PressureSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

func (s *pressureStore) snapshotLocked() PressureSnapshot {
	return PressureSnapshot{
		Sequence:   s.sequence,
		ObservedAt: s.observedAt,
		Bits:       s.bits,
	}
}

func (s *pressureStore) statsSnapshot(now time.Time, windowMinutes int) FloorStatsSnapshot {
	s.mu.Lock()
	minute := now.Unix() / 60
	s.advanceBucketsLocked(minute)
	s.mu.Unlock()

	s.mu.RLock()
	defer s.mu.RUnlock()

	var snapshot FloorStatsSnapshot
	snapshot.ActivePressedTiles = s.activePressedTiles
	nowNanos := now.UnixNano()

	if windowMinutes <= 0 || windowMinutes > historyMinutes {
		// Lifetime
		for index := 0; index < floor.TileCount; index++ {
			durationNs := s.tilePressedDurationNs[index]
			if start := s.tilePressedSince[index]; start > 0 && nowNanos > start {
				durationNs += uint64(nowNanos - start)
			}
			snapshot.Tiles[index] = TileStats{
				Presses:            s.tilePresses[index],
				PressedDurationSec: float64(durationNs) / 1e9,
			}
		}
		return snapshot
	}

	// Windowed (e.g. 5m, 15m)
	for index := 0; index < floor.TileCount; index++ {
		var count uint32
		var dwellNs uint64
		for w := 0; w < windowMinutes; w++ {
			bIdx := (minute - int64(w)) % historyMinutes
			if bIdx < 0 {
				bIdx += historyMinutes
			}
			count += s.minuteBuckets[bIdx][index]
			dwellNs += uint64(s.minuteDwellNs[bIdx][index])
		}
		if start := s.tilePressedSince[index]; start > 0 && nowNanos > start {
			dwellNs += uint64(nowNanos - start)
		}
		snapshot.Tiles[index] = TileStats{
			Presses:            uint64(count),
			PressedDurationSec: float64(dwellNs) / 1e9,
		}
	}
	return snapshot
}

func (s *pressureStore) resetStats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tilePresses = [floor.TileCount]uint64{}
	s.tilePressedDurationNs = [floor.TileCount]uint64{}
	s.minuteBuckets = [historyMinutes][floor.TileCount]uint32{}
	s.minuteDwellNs = [historyMinutes][floor.TileCount]uint32{}
	s.lastBucketMinute = 0
	for ch := range s.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *pressureStore) subscribe() (<-chan struct{}, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subscribers == nil {
		s.subscribers = make(map[chan struct{}]struct{})
	}
	ch := make(chan struct{}, 1)
	s.subscribers[ch] = struct{}{}
	unsubscribe := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.subscribers, ch)
	}
	return ch, unsubscribe
}

func replaceLatest[T any](channel chan T, value T) {
	select {
	case channel <- value:
		return
	default:
	}
	select {
	case <-channel:
	default:
	}
	select {
	case channel <- value:
	default:
	}
}
