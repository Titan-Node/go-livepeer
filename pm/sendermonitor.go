package pm

import (
	"fmt"
	"math/big"
	"sync"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/monitor"
	"github.com/pkg/errors"
)

// unixNow returns the current unix time
// This is a wrapper function that can be stubbed in tests
var unixNow = func() int64 {
	return time.Now().Unix()
}

// SenderMonitor is an interface that describes methods used to
// monitor remote senders
type SenderMonitor interface {
	// Start initiates the helper goroutines for the monitor
	Start()

	// Stop signals the monitor to exit gracefully
	Stop()

	// QueueTicket adds a ticket to the queue for a remote sender
	QueueTicket(ticket *SignedTicket) error

	// AddFloat adds to a remote sender's max float
	AddFloat(addr ethcommon.Address, amount *big.Int) error

	// SubFloat subtracts from a remote sender's max float
	SubFloat(addr ethcommon.Address, amount *big.Int)

	// MaxFloat returns a remote sender's max float
	MaxFloat(addr ethcommon.Address) (*big.Int, error)

	// ValidateSender checks whether a sender's unlock period ends the round after the next round
	ValidateSender(addr ethcommon.Address) error
}

type remoteSender struct {
	// pendingAmount is the sum of the face values of tickets that are
	// currently pending redemption on-chain
	pendingAmount *big.Int

	queue *ticketQueue

	done chan struct{}

	lastAccess int64
}

type senderMonitor struct {
	claimant        ethcommon.Address
	cleanupInterval time.Duration
	ttl             int

	mu      sync.Mutex
	senders map[ethcommon.Address]*remoteSender

	broker Broker
	smgr   SenderManager
	tm     TimeManager

	// redeemable is a channel that an external caller can use to
	// receive tickets that are fed from the ticket queues for
	// each of currently active remote senders
	redeemable chan *redemption

	ticketStore TicketStore

	quit chan struct{}
}

// NewSenderMonitor returns a new SenderMonitor
func NewSenderMonitor(claimant ethcommon.Address, broker Broker, smgr SenderManager, tm TimeManager, store TicketStore, cleanupInterval time.Duration, ttl int) SenderMonitor {
	return &senderMonitor{
		claimant:        claimant,
		cleanupInterval: cleanupInterval,
		ttl:             ttl,
		broker:          broker,
		smgr:            smgr,
		tm:              tm,
		senders:         make(map[ethcommon.Address]*remoteSender),
		redeemable:      make(chan *redemption),
		ticketStore:     store,
		quit:            make(chan struct{}),
	}
}

// Start initiates the helper goroutines for the monitor
func (sm *senderMonitor) Start() {
	go sm.startCleanupLoop()
}

// Stop signals the monitor to exit gracefully
func (sm *senderMonitor) Stop() {
	close(sm.quit)
}

// AddFloat adds to a remote sender's max float
func (sm *senderMonitor) AddFloat(addr ethcommon.Address, amount *big.Int) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.ensureCache(addr)

	// Subtracting from pendingAmount = adding to max float
	pendingAmount := sm.senders[addr].pendingAmount
	if pendingAmount.Cmp(amount) < 0 {
		return errors.New("cannot subtract from insufficient pendingAmount")
	}

	sm.senders[addr].pendingAmount.Sub(pendingAmount, amount)
	return nil
}

// SubFloat subtracts from a remote sender's max float
func (sm *senderMonitor) SubFloat(addr ethcommon.Address, amount *big.Int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.ensureCache(addr)

	// Adding to pendingAmount = subtracting from max float
	pendingAmount := sm.senders[addr].pendingAmount
	sm.senders[addr].pendingAmount.Add(pendingAmount, amount)
}

// MaxFloat returns a remote sender's max float
func (sm *senderMonitor) MaxFloat(addr ethcommon.Address) (*big.Int, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.ensureCache(addr)

	return sm.maxFloat(addr)
}

// QueueTicket adds a ticket to the queue for a remote sender
func (sm *senderMonitor) QueueTicket(ticket *SignedTicket) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.ensureCache(ticket.Sender)

	return sm.senders[ticket.Sender].queue.Add(ticket)
}

// ValidateSender checks whether a sender's unlock period ends the round after the next round
func (sm *senderMonitor) ValidateSender(addr ethcommon.Address) error {
	info, err := sm.smgr.GetSenderInfo(addr)
	if err != nil {
		return fmt.Errorf("could not get sender info for %v: %v", addr.Hex(), err)
	}
	maxWithdrawRound := new(big.Int).Add(sm.tm.LastInitializedRound(), big.NewInt(1))
	if info.WithdrawRound.Int64() != 0 && info.WithdrawRound.Cmp(maxWithdrawRound) != 1 {
		return fmt.Errorf("deposit and reserve for sender %v is set to unlock soon", addr.Hex())
	}
	return nil
}

// maxFloat is a helper that returns the sender's max float as:
// reserveAlloc - pendingAmount
// Caller should hold the lock for senderMonitor
func (sm *senderMonitor) maxFloat(addr ethcommon.Address) (*big.Int, error) {
	reserveAlloc, err := sm.reserveAlloc(addr)
	if err != nil {
		return nil, err
	}
	return new(big.Int).Sub(reserveAlloc, sm.senders[addr].pendingAmount), nil
}

func (sm *senderMonitor) reserveAlloc(addr ethcommon.Address) (*big.Int, error) {
	info, err := sm.smgr.GetSenderInfo(addr)
	if err != nil {
		return nil, err
	}
	claimed, err := sm.smgr.ClaimedReserve(addr, sm.claimant)
	poolSize := sm.tm.GetTranscoderPoolSize()
	if poolSize.Cmp(big.NewInt(0)) == 0 {
		return big.NewInt(0), nil
	}
	reserve := new(big.Int).Add(info.Reserve.FundsRemaining, info.Reserve.ClaimedInCurrentRound)
	return new(big.Int).Sub(new(big.Int).Div(reserve, poolSize), claimed), nil
}

// ensureCache is a helper that checks if a remote sender is initialized
// and if not will fetch and cache the remote sender's reserve alloc
// Caller should hold the lock for senderMonitor
func (sm *senderMonitor) ensureCache(addr ethcommon.Address) {
	if sm.senders[addr] == nil {
		sm.cache(addr)
	}

	sm.senders[addr].lastAccess = unixNow()
}

// cache is a helper that caches a remote sender's reserve alloc and
// starts a ticket queue for the remote sender
// Caller should hold the lock for senderMonitor unless the caller is
// ensureCache() in which case the caller of ensureCache() should hold the lock
func (sm *senderMonitor) cache(addr ethcommon.Address) {
	queue := newTicketQueue(sm.ticketStore, addr, sm.tm.SubscribeBlocks)
	queue.Start()
	done := make(chan struct{})
	go sm.startTicketQueueConsumerLoop(queue, done)

	sm.senders[addr] = &remoteSender{
		pendingAmount: big.NewInt(0),
		queue:         queue,
		done:          done,
		lastAccess:    unixNow(),
	}
}

// startTicketQueueConsumerLoop initiates a loop that runs a consumer
// that receives redeemable tickets from a ticketQueue and feeds them into
// a single output channel in a fan-in manner
func (sm *senderMonitor) startTicketQueueConsumerLoop(queue *ticketQueue, done chan struct{}) {
	for {
		select {
		case ticket := <-queue.Redeemable():
			if err := sm.redeemWinningTicket(ticket); err != nil {
				glog.Errorf("error redeeming err=%v", err)
			}
		case <-done:
			// When the ticket consumer exits, tell the ticketQueue
			// to exit as well
			queue.Stop()

			return
		case <-sm.quit:
			// When the monitor exits, tell the ticketQueue
			// to exit as well
			queue.Stop()

			return
		}
	}
}

// startCleanupLoop initiates a loop that runs a cleanup worker
// every cleanupInterval
func (sm *senderMonitor) startCleanupLoop() {
	ticker := time.NewTicker(sm.cleanupInterval)

	for {
		select {
		case <-ticker.C:
			sm.cleanup()
		case <-sm.quit:
			return
		}
	}
}

// cleanup removes tracked remote senders that have exceeded
// their ttl
func (sm *senderMonitor) cleanup() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for k, v := range sm.senders {
		if unixNow()-v.lastAccess > int64(sm.ttl) {
			// Signal the ticket queue consumer to exit gracefully
			v.done <- struct{}{}

			delete(sm.senders, k)
			sm.smgr.Clear(k)
		}
	}
}

func (sm *senderMonitor) redeemWinningTicket(ticket *SignedTicket) (err error) {
	maxFloat, e := sm.MaxFloat(ticket.Ticket.Sender)
	if e != nil {
		err = e
		return
	}

	// if max float is zero, there is no claimable reserve left or reserve is 0
	if maxFloat.Cmp(big.NewInt(0)) <= 0 {
		sm.QueueTicket(ticket)
		err = errors.Errorf("max float is zero")
		return
	}

	// If max float is insufficient to cover the ticket face value, queue
	// the ticket to be retried later
	if maxFloat.Cmp(ticket.Ticket.FaceValue) < 0 {
		sm.QueueTicket(ticket)
		err = fmt.Errorf("insufficient max float sender=%v faceValue=%v maxFloat=%v", ticket.Ticket.Sender.Hex(), ticket.Ticket.FaceValue, maxFloat)
		return
	}

	// Subtract the ticket face value from the sender's current max float
	// This amount will be considered pending until the ticket redemption
	// transaction confirms on-chain
	sm.SubFloat(ticket.Ticket.Sender, ticket.Ticket.FaceValue)

	defer func() {
		// Add the ticket face value back to the sender's current max float
		// This amount is no longer considered pending since the ticket
		// redemption transaction either confirmed on-chain or was not
		// submitted at all
		//
		// TODO(yondonfu): Should ultimately add back only the amount that
		// was actually successfully redeemed in order to take into account
		// the case where the ticket was not redeemd for its full face value
		// because the reserve was insufficient
		e := sm.AddFloat(ticket.Ticket.Sender, ticket.Ticket.FaceValue)
		if e != nil {
			err = e
		}
	}()

	// Assume that that this call will return immediately if there
	// is an error in transaction submission
	tx, e := sm.broker.RedeemWinningTicket(ticket.Ticket, ticket.Sig, ticket.RecipientRand)
	if e != nil {
		if monitor.Enabled {
			monitor.TicketRedemptionError(ticket.Ticket.Sender.String())
		}
		err = e
		return
	}

	// Wait for transaction to confirm
	if e := sm.broker.CheckTx(tx); e != nil {
		if monitor.Enabled {
			monitor.TicketRedemptionError(ticket.Ticket.Sender.String())
		}

		err = e
		return
	}

	if monitor.Enabled {
		// TODO(yondonfu): Handle case where < ticket.FaceValue is actually
		// redeemed i.e. if sender reserve cannot cover the full ticket.FaceValue
		monitor.ValueRedeemed(ticket.Ticket.Sender.String(), ticket.Ticket.FaceValue)
	}

	return
}
