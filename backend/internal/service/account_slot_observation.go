package service

import (
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// AccountSlotObservation is the process-local source of truth for the live
// account slots shown by the operations dashboard. It deliberately does not
// share the scheduler's Redis keys: the scheduler decides admission, while
// this collection records the requests that are actually in flight.
type AccountSlotObservation struct {
	Active  int64            `json:"active"`
	Waiting int64            `json:"waiting"`
	Users   map[string]int64 `json:"users"`
}

type observedAccountSlots struct {
	slots   map[uint64]int64
	waiting int64
}

type accountSlotObservationStore struct {
	mu       sync.RWMutex
	accounts map[int64]*observedAccountSlots
	nextID   atomic.Uint64
}

func newAccountSlotObservationStore() *accountSlotObservationStore {
	return &accountSlotObservationStore{accounts: make(map[int64]*observedAccountSlots)}
}

func (s *accountSlotObservationStore) acquire(accountID, userID int64) func() {
	if s == nil || accountID <= 0 {
		return func() {}
	}
	token := s.nextID.Add(1)
	s.mu.Lock()
	account := s.accounts[accountID]
	if account == nil {
		account = &observedAccountSlots{slots: make(map[uint64]int64)}
		s.accounts[accountID] = account
	}
	account.slots[token] = userID
	s.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			account := s.accounts[accountID]
			if account != nil {
				delete(account.slots, token)
				if len(account.slots) == 0 && account.waiting == 0 {
					delete(s.accounts, accountID)
				}
			}
			s.mu.Unlock()
		})
	}
}

func (s *accountSlotObservationStore) incrementWaiting(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.mu.Lock()
	account := s.accounts[accountID]
	if account == nil {
		account = &observedAccountSlots{slots: make(map[uint64]int64)}
		s.accounts[accountID] = account
	}
	account.waiting++
	s.mu.Unlock()
}

func (s *accountSlotObservationStore) decrementWaiting(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.mu.Lock()
	account := s.accounts[accountID]
	if account != nil {
		if account.waiting > 0 {
			account.waiting--
		}
		if len(account.slots) == 0 && account.waiting == 0 {
			delete(s.accounts, accountID)
		}
	}
	s.mu.Unlock()
}

func (s *accountSlotObservationStore) snapshot() map[int64]AccountSlotObservation {
	result := make(map[int64]AccountSlotObservation)
	if s == nil {
		return result
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for accountID, account := range s.accounts {
		if account == nil {
			continue
		}
		users := make(map[string]int64)
		for _, userID := range account.slots {
			if userID > 0 {
				key := strconv.FormatInt(userID, 10)
				users[key]++
			}
		}
		result[accountID] = AccountSlotObservation{
			Active:  int64(len(account.slots)),
			Waiting: account.waiting,
			Users:   users,
		}
	}
	return result
}

// SnapshotAccountSlotObservation returns a copy that can be serialized safely
// without holding the store lock during HTTP response encoding.
func (s *ConcurrencyService) SnapshotAccountSlotObservation() (map[int64]AccountSlotObservation, time.Time) {
	if s == nil || s.observation == nil {
		return map[int64]AccountSlotObservation{}, time.Now().UTC()
	}
	return s.observation.snapshot(), time.Now().UTC()
}
