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
	Active       int64            `json:"active"`
	Waiting      int64            `json:"waiting"`
	Users        map[string]int64 `json:"users"`
	WaitingUsers map[string]int64 `json:"waiting_users"`
}

type observedAccountSlots struct {
	slots        map[uint64]int64
	sessions     map[uint64]observedAccountSession
	waiting      int64
	waitingUsers map[int64]int64
}

type observedAccountSession struct {
	groupID int64
	hash    string
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
	return s.acquireSession(accountID, userID, 0, "")
}

func (s *accountSlotObservationStore) acquireSession(accountID, userID, groupID int64, hash string) func() {
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
	if hash != "" {
		if account.sessions == nil {
			account.sessions = make(map[uint64]observedAccountSession)
		}
		account.sessions[token] = observedAccountSession{groupID: groupID, hash: hash}
	}
	s.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			account := s.accounts[accountID]
			if account != nil {
				delete(account.slots, token)
				delete(account.sessions, token)
				if len(account.slots) == 0 && account.waiting == 0 {
					delete(s.accounts, accountID)
				}
			}
			s.mu.Unlock()
		})
	}
}

func (s *ConcurrencyService) dispatchSessionInFlight(accountID, userID, groupID int64, hash string) int64 {
	if s == nil || s.observation == nil {
		return 0
	}
	s.observation.mu.RLock()
	defer s.observation.mu.RUnlock()
	account := s.observation.accounts[accountID]
	if account == nil {
		return 0
	}
	var count int64
	for token, session := range account.sessions {
		if account.slots[token] == userID && session.groupID == groupID && session.hash == hash {
			count++
		}
	}
	return count
}

func (s *accountSlotObservationStore) incrementWaiting(accountID, userID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	if userID <= 0 {
		userID = 0
	}
	s.mu.Lock()
	account := s.accounts[accountID]
	if account == nil {
		account = &observedAccountSlots{slots: make(map[uint64]int64)}
		s.accounts[accountID] = account
	}
	if account.waitingUsers == nil {
		account.waitingUsers = make(map[int64]int64)
	}
	// 等待计数按请求累加；身份缺失单独保留，不能归到正在使用账号的其他用户。
	account.waitingUsers[userID]++
	account.waiting++
	s.mu.Unlock()
}

func (s *accountSlotObservationStore) decrementWaiting(accountID, userID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	if userID <= 0 {
		userID = 0
	}
	s.mu.Lock()
	account := s.accounts[accountID]
	if account != nil {
		// 仅移除本用户的等待请求，避免缺失身份或重复清理误扣其他用户的队列。
		if count := account.waitingUsers[userID]; count > 0 {
			if count == 1 {
				delete(account.waitingUsers, userID)
			} else {
				account.waitingUsers[userID] = count - 1
			}
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
		waitingUsers := make(map[string]int64)
		for userID, count := range account.waitingUsers {
			if userID > 0 {
				waitingUsers[strconv.FormatInt(userID, 10)] = count
			}
		}
		result[accountID] = AccountSlotObservation{
			Active:       int64(len(account.slots)),
			Waiting:      account.waiting,
			Users:        users,
			WaitingUsers: waitingUsers,
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
