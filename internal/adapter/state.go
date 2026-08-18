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
// replacement takes the write lock, so once replacement completes no physical
// transaction can still be using a frame from the retired generation.
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
	TotalPresses       uint64
	PressedDurationSec float64
	Tiles              [floor.TileCount]TileStats
}

type minuteStatsBucket struct {
	valid   bool
	minute  int64
	presses [floor.TileCount]uint32
	dwellNS [floor.TileCount]uint64
}

type pressureStore struct {
	mu         sync.RWMutex
	sequence   uint64
	observedAt time.Time

	// Physical and diagnostic input are independent layers. Canonical pressure
	// is their union, so expiry/release of a simulated touch can never clear a
	// tile that remains physically pressed.
	physicalBits   [floor.PressureByteCount]byte
	debugBits      [floor.PressureByteCount]byte
	debugExpiresAt [floor.TileCount]time.Time
	bits           [floor.PressureByteCount]byte

	activePressedTiles    uint32
	tilePresses           [floor.TileCount]uint64
	tilePressedDurationNS [floor.TileCount]uint64
	tilePressedSince      [floor.TileCount]time.Time
	minuteBuckets         [historyMinutes]minuteStatsBucket

	subscribers map[chan struct{}]struct{}
}

func bitIsSet(bits *[floor.PressureByteCount]byte, index int) bool {
	return bits[index/8]&(1<<uint(index%8)) != 0
}

func setBit(bits *[floor.PressureByteCount]byte, index int, pressed bool) {
	mask := byte(1 << uint(index%8))
	if pressed {
		bits[index/8] |= mask
		return
	}
	bits[index/8] &^= mask
}

func (s *pressureStore) bucketLocked(minute int64) *minuteStatsBucket {
	index := int(minute % historyMinutes)
	if index < 0 {
		index += historyMinutes
	}
	bucket := &s.minuteBuckets[index]
	if !bucket.valid || bucket.minute != minute {
		*bucket = minuteStatsBucket{valid: true, minute: minute}
	}
	return bucket
}

func (s *pressureStore) addDwellToBucketsLocked(tile int, start, end time.Time) {
	if !end.After(start) {
		return
	}
	endMinute := end.Add(-time.Nanosecond).Unix() / 60
	startMinute := start.Unix() / 60
	earliestMinute := endMinute - historyMinutes + 1
	if startMinute < earliestMinute {
		startMinute = earliestMinute
	}
	for minute := startMinute; minute <= endMinute; minute++ {
		bucketStart := time.Unix(minute*60, 0)
		bucketEnd := bucketStart.Add(time.Minute)
		overlapStart := start
		if overlapStart.Before(bucketStart) {
			overlapStart = bucketStart
		}
		overlapEnd := end
		if overlapEnd.After(bucketEnd) {
			overlapEnd = bucketEnd
		}
		if overlapEnd.After(overlapStart) {
			s.bucketLocked(minute).dwellNS[tile] += uint64(overlapEnd.Sub(overlapStart))
		}
	}
}

func (s *pressureStore) notifyLocked() {
	for subscriber := range s.subscribers {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

func (s *pressureStore) updateCanonicalLocked(index int, observedAt time.Time) bool {
	wasPressed := bitIsSet(&s.bits, index)
	pressed := bitIsSet(&s.physicalBits, index) || bitIsSet(&s.debugBits, index)
	if wasPressed == pressed {
		return false
	}

	setBit(&s.bits, index, pressed)
	if pressed {
		s.tilePresses[index]++
		s.bucketLocked(observedAt.Unix() / 60).presses[index]++
		s.tilePressedSince[index] = observedAt
		s.activePressedTiles++
		return true
	}

	if start := s.tilePressedSince[index]; !start.IsZero() {
		if observedAt.After(start) {
			duration := observedAt.Sub(start)
			s.tilePressedDurationNS[index] += uint64(duration)
			s.addDwellToBucketsLocked(index, start, observedAt)
		}
		s.tilePressedSince[index] = time.Time{}
	}
	if s.activePressedTiles > 0 {
		s.activePressedTiles--
	}
	return true
}

func (s *pressureStore) applyLayer(changes []pressureChange, observedAt time.Time, debug bool, lease time.Duration) (PressureSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	canonicalChanged := false
	for _, change := range changes {
		if !floor.InLogicalBounds(change.X, change.Y) {
			continue
		}
		index := change.Y*floor.GridWidth + change.X
		layer := &s.physicalBits
		if debug {
			layer = &s.debugBits
			if change.Pressed {
				s.debugExpiresAt[index] = observedAt.Add(lease)
			} else {
				s.debugExpiresAt[index] = time.Time{}
			}
		}
		if bitIsSet(layer, index) == change.Pressed {
			// Repeated simulated press messages still renew their lease.
			continue
		}
		setBit(layer, index, change.Pressed)
		canonicalChanged = s.updateCanonicalLocked(index, observedAt) || canonicalChanged
	}
	if !canonicalChanged {
		return s.snapshotLocked(), false
	}

	s.sequence++
	s.observedAt = observedAt
	s.notifyLocked()
	return s.snapshotLocked(), true
}

// apply is retained as the physical-input operation used throughout the core
// adapter and existing characterization tests.
func (s *pressureStore) apply(changes []pressureChange, observedAt time.Time) (PressureSnapshot, bool) {
	return s.applyLayer(changes, observedAt, false, 0)
}

func (s *pressureStore) applyDebug(changes []pressureChange, observedAt time.Time, lease time.Duration) (PressureSnapshot, bool) {
	return s.applyLayer(changes, observedAt, true, lease)
}

func (s *pressureStore) expireDebug(now time.Time) (PressureSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	canonicalChanged := false
	for index, expiresAt := range s.debugExpiresAt {
		if expiresAt.IsZero() || now.Before(expiresAt) {
			continue
		}
		s.debugExpiresAt[index] = time.Time{}
		if !bitIsSet(&s.debugBits, index) {
			continue
		}
		setBit(&s.debugBits, index, false)
		canonicalChanged = s.updateCanonicalLocked(index, now) || canonicalChanged
	}
	if !canonicalChanged {
		return s.snapshotLocked(), false
	}

	s.sequence++
	s.observedAt = now
	s.notifyLocked()
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
	s.mu.RLock()
	defer s.mu.RUnlock()

	var snapshot FloorStatsSnapshot
	snapshot.ActivePressedTiles = s.activePressedTiles

	if windowMinutes <= 0 || windowMinutes > historyMinutes {
		for index := 0; index < floor.TileCount; index++ {
			durationNS := s.tilePressedDurationNS[index]
			if start := s.tilePressedSince[index]; !start.IsZero() && now.After(start) {
				durationNS += uint64(now.Sub(start))
			}
			stats := TileStats{
				Presses:            s.tilePresses[index],
				PressedDurationSec: float64(durationNS) / float64(time.Second),
			}
			snapshot.Tiles[index] = stats
			snapshot.TotalPresses += stats.Presses
			snapshot.PressedDurationSec += stats.PressedDurationSec
		}
		return snapshot
	}

	currentMinute := now.Unix() / 60
	earliestMinute := currentMinute - int64(windowMinutes) + 1
	windowStart := now.Add(-time.Duration(windowMinutes) * time.Minute)
	for index := 0; index < floor.TileCount; index++ {
		var presses uint64
		var dwellNS uint64
		for minute := earliestMinute; minute <= currentMinute; minute++ {
			bucketIndex := int(minute % historyMinutes)
			if bucketIndex < 0 {
				bucketIndex += historyMinutes
			}
			bucket := &s.minuteBuckets[bucketIndex]
			if bucket.valid && bucket.minute == minute {
				presses += uint64(bucket.presses[index])
				dwellNS += bucket.dwellNS[index]
			}
		}
		if start := s.tilePressedSince[index]; !start.IsZero() && now.After(start) {
			overlapStart := start
			if overlapStart.Before(windowStart) {
				overlapStart = windowStart
			}
			if now.After(overlapStart) {
				dwellNS += uint64(now.Sub(overlapStart))
			}
		}
		stats := TileStats{
			Presses:            presses,
			PressedDurationSec: float64(dwellNS) / float64(time.Second),
		}
		snapshot.Tiles[index] = stats
		snapshot.TotalPresses += stats.Presses
		snapshot.PressedDurationSec += stats.PressedDurationSec
	}
	return snapshot
}

// resetStats clears only diagnostic counters. Canonical pressure remains
// untouched, and any currently held tile starts accruing new dwell from now.
func (s *pressureStore) resetStats(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.tilePresses = [floor.TileCount]uint64{}
	s.tilePressedDurationNS = [floor.TileCount]uint64{}
	s.minuteBuckets = [historyMinutes]minuteStatsBucket{}
	for index := 0; index < floor.TileCount; index++ {
		if bitIsSet(&s.bits, index) {
			s.tilePressedSince[index] = now
		} else {
			s.tilePressedSince[index] = time.Time{}
		}
	}
	s.notifyLocked()
}

func (s *pressureStore) subscribe() (<-chan struct{}, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subscribers == nil {
		s.subscribers = make(map[chan struct{}]struct{})
	}
	channel := make(chan struct{}, 1)
	s.subscribers[channel] = struct{}{}
	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subscribers, channel)
			s.mu.Unlock()
		})
	}
	return channel, unsubscribe
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
