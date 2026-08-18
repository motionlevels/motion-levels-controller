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

type pressureStore struct {
	mu         sync.RWMutex
	sequence   uint64
	observedAt time.Time
	bits       [floor.PressureByteCount]byte
}

func (s *pressureStore) apply(changes []pressureChange, observedAt time.Time) (PressureSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
		} else {
			s.bits[index/8] &^= mask
		}
		changed = true
	}
	if !changed {
		return s.snapshotLocked(), false
	}
	s.sequence++
	s.observedAt = observedAt
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
