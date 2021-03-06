package gobft

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coschain/gobft/common"
	"github.com/coschain/gobft/custom"
	"github.com/coschain/gobft/message"
	"github.com/sirupsen/logrus"
)

type Core struct {
	name       string
	cfg        *Config
	validators *Validators

	RoundState
	stateSync *StateSync
	//triggeredTimeoutPrecommit bool
	hasRecvCommitRecords bool

	msgQueue      chan msgInfo
	timeoutTicker TimeoutTicker
	started       int32
	inStartOrStop int32
	done          chan struct{}
	inFetch       bool

	msgCnt         int64
	msgProcessTime time.Duration

	extLog *logrus.Logger
	log    *logrus.Entry

	sync.RWMutex
	sync.WaitGroup

	// for test only
	byzantinePrevote *message.ProposedData
}

func NewCore(vals custom.ICommittee, pVal custom.IPrivValidator) *Core {
	c := &Core{
		cfg:        DefaultConfig(),
		validators: NewValidators(vals, pVal),
		msgQueue:   make(chan msgInfo, msgQueueSize),
		started:    0,
	}
	//c.cfg.SkipTimeoutCommit = true
	c.stateSync = NewStateSync(c)

	return c
}

func (c *Core) SetLogger(lg *logrus.Logger) {
	c.extLog = lg
	c.log = lg.WithField("gobft", "on")
	//lg.SetLevel(logrus.DebugLevel)

	//logrus.SetLevel(logrus.Level(lv))
}

func (c *Core) SetName(n string) {
	c.name = n
	c.log = c.extLog.WithFields(logrus.Fields{
		"gobft":    "on",
		"CoreName": n,
	})
}

func (c *Core) Start() error {
	if !atomic.CompareAndSwapInt32(&c.inStartOrStop, 0, 1) {
		return errors.New("gobft is in the process of start or stop")
	}
	defer atomic.StoreInt32(&c.inStartOrStop, 0)

	if atomic.LoadInt32(&c.started) == 1 {
		return errors.New("gobft already started")
	}

	c.done = make(chan struct{})
	c.timeoutTicker = NewTimeoutTicker(c)

	if err := c.timeoutTicker.Start(); err != nil {
		return err
	}
	appState := c.validators.CustomValidators.GetAppState()
	c.Votes = nil
	c.updateToAppState(appState)

	c.Add(1)
	go c.receiveRoutine()
	c.StartTime = time.Now().Add(time.Second)
	c.scheduleRound0(c.GetRoundState())
	atomic.StoreInt32(&c.started, 1)
	return nil
}

func (c *Core) Stop() error {
	if !atomic.CompareAndSwapInt32(&c.inStartOrStop, 0, 1) {
		return errors.New("gobft is in the process of start or stop")
	}
	defer atomic.StoreInt32(&c.inStartOrStop, 0)

	if atomic.LoadInt32(&c.started) == 0 {
		return errors.New("gobft already stopped")
	}

	c.timeoutTicker.Stop()
	close(c.done)
	c.Wait()
	c.log.Info("bftCore stopped")
	atomic.StoreInt32(&c.started, 0)
	return nil
}

// GetRoundState returns a shallow copy of the internal consensus state.
func (c *Core) GetRoundState() *RoundState {
	c.RLock()
	rs := c.RoundState // copy
	c.RUnlock()
	return &rs
}

func (c *Core) GetLastCommit() *message.Commit {
	c.RLock()
	defer c.RUnlock()

	return c.LastCommit.MakeCommit()
}

// RecvMsg accepts a ConsensusMessage and delivers it to receiveRoutine
func (c *Core) RecvMsg(msg message.ConsensusMessage, p custom.IPeer) error {
	if atomic.LoadInt32(&c.started) == 1 {
		if err := msg.ValidateBasic(); err != nil {
			c.log.Error(err)
			return err
		}
		c.sendInternalMessage(msgInfo{msg, p})
	} else {
		return errors.New("gobft is not running")
	}
	return nil
}

// enterNewRound(height, 0) at c.StartTime.
func (c *Core) scheduleRound0(rs *RoundState) {
	c.log.Info("scheduleRound0", " now ", common.Now(), " startTime ", c.StartTime,
		" (", rs.Height, "/", 0, "/", RoundStepNewHeight, ")")
	sleepDuration := rs.StartTime.Sub(common.Now()) // nolint: gotype, gosimple
	c.scheduleTimeout(sleepDuration, rs.Height, 0, RoundStepNewHeight)
}

func (c *Core) updateRoundStep(round int, step RoundStepType) {
	c.Round = round
	c.Step = step
	c.inFetch = false
}

func (c *Core) updateToAppState(appState *message.AppState) {
	if appState == nil {
		return
	}

	if c.CommitRound > -1 && 0 < c.Height && c.Height > appState.LastHeight {
		// appState height might proceed when gobft routine is stopped
		common.PanicSanity(fmt.Sprintf("updateToState() expected state height of %v but found %v. CORE %s",
			c.Height, appState.LastHeight, c.name))
	}

	var lastPrecommits *VoteSet = nil
	if c.CommitRound > -1 && c.Votes != nil {
		if !c.Votes.Precommits(c.CommitRound).HasTwoThirdsMajority() {
			common.PanicSanity("updateToState(state) called but last Precommit round didn't have +2/3. CORE  " + c.name)
		}
		lastPrecommits = c.Votes.Precommits(c.CommitRound)
	}

	// Next desired bft height
	c.Height = appState.LastHeight + 1
	c.updateRoundStep(0, RoundStepNewHeight)
	if c.CommitTime.IsZero() {
		// "Now" makes it easier to sync up dev nodes.
		// We add timeoutCommit to allow transactions
		// to be gathered for the first block.
		// And alternative solution that relies on clocks:
		//  c.StartTime = state.LastBlockTime.Add(timeoutCommit)
		c.StartTime = c.cfg.Commit(common.Now())
	} else {
		c.StartTime = c.cfg.Commit(c.CommitTime)
	}

	c.Proposal = nil
	c.LockedRound = -1
	c.LockedProposal = nil

	c.CommitRound = -1
	c.LastCommit = lastPrecommits
	c.lastCommittedData = appState.LastProposedData
	c.Votes = NewHeightVoteSet(c.Height, c.validators, &c.lastCommittedData)
}

// receiveRoutine keeps the RoundState and is the only thing that updates it.
// Updates (state transitions) happen on timeouts, complete proposals, and 2/3 majorities.
// Core must be locked before any internal state is updated.
func (c *Core) receiveRoutine() {
	defer c.Done()
	for {
		rs := c.RoundState
		var mi msgInfo

		select {
		case <-c.done:
			return
		case mi = <-c.msgQueue:
			startTime := time.Now()
			c.handleMsg(mi)
			elapsed := time.Since(startTime)
			c.msgProcessTime += elapsed
			c.msgCnt++
			if c.msgCnt % 100 == 0 {
				c.log.Infof("average time to process a consensus msg: %d ms",
					c.msgProcessTime.Nanoseconds()/1e6/c.msgCnt)
			}
		case ti := <-c.timeoutTicker.Chan(): // tockChan:
			c.handleTimeout(ti, rs)
		}
	}
}

func (c *Core) handleMsg(mi msgInfo) {
	c.Lock()
	defer c.Unlock()

	c.log.Debug("handleMsg: ", mi.Msg)
	var err error
	msg := mi.Msg

	switch msg := msg.(type) {
	case *message.Vote:
		// if we're not a validator, just ignore
		if !c.isValidator() { // TODO: cache
			return
		}

		_, err = c.tryAddVote(msg)

		if err == ErrAddingVote {
			// TODO: punish peer
			// We probably don't want to stop the peer here. The vote does not
			// necessarily comes from a malicious peer but can be just broadcasted by
			// a typical peer.
			// https://github.com/tendermint/tendermint/issues/1281
		}

		// NOTE: the vote is broadcast to peers by the reactor listening
		// for vote events

		// TODO: If rs.Height == vote.Height && rs.Round < vote.Round,
		// the peer is sending us CatchupCommit precommits.
		// We could make note of this and help filter in broadcastHasVoteMessage().
	// case *message.FetchVotesReq:

	case *message.Commit:
		//c.log.Debug("handle Commit: ", msg)
		if err := msg.ValidateBasic(); err != nil {
			c.log.Error(err)
			return
		}
		if msg.Height() == c.Height {
			c.hasRecvCommitRecords = true
		}
		if msg.Height() >= c.Height {
			for i := range msg.Precommits {
				c.tryAddVote(msg.Precommits[i])
			}
		}

	case *message.FetchVotesReq:
		c.handleFetch(msg, mi.Peer)
	case *message.FetchVotesRsp:
		c.handleFetchRsp(msg, mi.Peer)
	default:
		c.log.Error("Unknown msg type ", reflect.TypeOf(msg))
	}
	if err != nil {
		c.log.Error("Error with msg ", " height ", c.Height, " round ", c.Round, " type ", reflect.TypeOf(msg), " err ", err, " msg ", msg)
	}
}

func (c *Core) handleFetch(msg *message.FetchVotesReq, p custom.IPeer) {
	c.log.Debug("handle FetchVotesReq: ", msg)
	if err := msg.ValidateBasic(); err != nil {
		c.log.Error(err)
		return
	}

	var rsp *message.FetchVotesRsp
	if msg.Height < c.Height {
		commit := c.validators.CustomValidators.GetCommitHistory(msg.Height)
		if commit == nil {
			c.log.Error("failed to get history commits")
			return
		}
		rsp = &message.FetchVotesRsp{
			Type:         message.PrecommitType,
			Height:       msg.Height,
			Round:        msg.Round,
			MissingVotes: commit.Precommits,
		}

	} else if msg.Height == c.Height && msg.Round <= c.Round {
		if msg.Type == message.PrevoteType {
			rsp = c.Votes.Prevotes(msg.Round).MakeFetchVotesRsp(msg)
		} else if msg.Type == message.PrecommitType {
			rsp = c.Votes.Precommits(msg.Round).MakeFetchVotesRsp(msg)
		}
	}
	if rsp != nil {
		c.validators.Sign(rsp)
		c.log.Debug("sending FetchVotesRsp", rsp)
		c.validators.CustomValidators.Send(rsp, p)
	}
}

func (c *Core) handleFetchRsp(msg *message.FetchVotesRsp, p custom.IPeer) {
	if c.Height == msg.Height && c.Round <= msg.Round {
		if c.Step != RoundStepPrevoteFetch && c.Step != RoundStepPrecommitFetch {
			return
		}
		if err := msg.ValidateBasic(); err != nil {
			c.log.Error(err)
			return
		}
		for i := range msg.MissingVotes {
			c.tryAddVote(msg.MissingVotes[i])
		}
	}
}

func (c *Core) handleTimeout(ti timeoutInfo, rs RoundState) {
	c.log.Debug("Received tock ", " timeout ", ti.Duration, " height ", ti.Height, " round ", ti.Round, " step ", ti.Step)

	// timeouts must be for current height, round, step
	if ti.Height != rs.Height || ti.Round < rs.Round || (ti.Round == rs.Round && ti.Step < rs.Step) {
		c.log.Debug("Ignoring tock because we're ahead ", " height ", rs.Height, " round ", rs.Round, " step ", rs.Step)
		return
	}

	// the timeout will now cause a state transition
	c.Lock()
	defer c.Unlock()

	switch ti.Step {
	case RoundStepNewHeight:
		// NewRound event fired from enterNewRound.
		// XXX: should we fire timeout here (for timeout commit)?
		c.enterNewRound(ti.Height, 0)
	case RoundStepNewRound:
		c.enterPropose(ti.Height, 0)
	case RoundStepPropose:
		c.enterPrevote(ti.Height, ti.Round)
	case RoundStepPrevoteFetch:
		c.fetchMissingVotes()
	case RoundStepPrevoteWait:
		c.enterPrecommit(ti.Height, ti.Round)
	case RoundStepPrecommitFetch:
		c.fetchMissingVotes()
	case RoundStepPrecommitWait:
		c.enterPrecommit(ti.Height, ti.Round)
		c.enterNewRound(ti.Height, ti.Round+1)
	default:
		panic(fmt.Sprintf("Invalid timeout step: %v", ti.Step))
	}

}

// State functions
// Used internally by handleTimeout and handleMsg to make state transitions

// Enter: `timeoutNewHeight` by startTime (commitTime+timeoutCommit),
// 	or, if SkipTimeout==true, after receiving all precommits from (height,round-1)
// Enter: `timeoutPrecommits` after any +2/3 precommits from (height,round-1)
// Enter: +2/3 precommits for nil at (height,round-1)
// Enter: +2/3 prevotes any or +2/3 precommits for block or any from (height, round)
// NOTE: c.StartTime was already set for height.
func (c *Core) enterNewRound(height int64, round int) {
	if c.Height != height || round < c.Round || (c.Round == round && c.Step != RoundStepNewHeight) {
		c.log.Debug(fmt.Sprintf("enterNewRound(%v/%v): Invalid args. Current step: %v/%v/%v", height, round, c.Height, c.Round, c.Step))
		return
	}

	if now := common.Now(); c.StartTime.After(now) {
		c.log.Info("Need to set a buffer and c.log message here for sanity.", "startTime", c.StartTime, "now", now)
	}

	c.log.Info(fmt.Sprintf("enterNewRound(%v/%v). Current: %v/%v/%v", height, round, c.Height, c.Round, c.Step))

	// Setup new round
	// we don't fire newStep for this step,
	// but we fire an event, so update the round step first
	c.updateRoundStep(round, RoundStepNewRound)
	if round == 0 {
		// We've already reset these upon new height,
		// and meanwhile we might have received a proposal
		// for round 0.
	} else {
		c.log.Infof("Resetting Proposal info, height %d, round %d", height, round)
		c.Proposal = nil
	}
	c.Votes.SetRound(round + 1) // also track next round (round+1) to allow round-skipping

	c.enterPropose(height, round)
}

func (c *Core) isReadyToPrevote() bool {
	// TODO:
	if c.Proposal != nil || c.LockedRound >= 0 {
		return true
	}

	return false
}

func (c *Core) isValidator() bool {
	self := c.validators.GetSelfPubKey()
	return c.validators.CustomValidators.IsValidator(self)
}

func (c *Core) enterPropose(height int64, round int) {
	if c.Height != height || round < c.Round || (c.Round == round && RoundStepPropose <= c.Step) {
		c.log.Debug(fmt.Sprintf("enterPropose(%v/%v): Invalid args. Current step: %v/%v/%v", height, round, c.Height, c.Round, c.Step))
		return
	}
	c.log.Info(fmt.Sprintf("enterPropose(%v/%v). Current: %v/%v/%v", height, round, c.Height, c.Round, c.Step))

	defer func() {
		// Done enterPropose:
		c.updateRoundStep(round, RoundStepPropose)

		// If we have the whole proposal + POL, then goto Prevote now.
		// else, we'll enterPrevote when the rest of the proposal is received (in AddProposalBlockPart),
		// or else after timeoutPropose
		if c.isReadyToPrevote() {
			c.enterPrevote(height, c.Round)
		}
	}()

	// If we don't get the proposal quick enough, enterPrevote
	c.scheduleTimeout(c.cfg.Propose(round), height, round, RoundStepPropose)

	self := c.validators.GetSelfPubKey()
	// Nothing more to do if we're not a validator
	if !c.validators.CustomValidators.IsValidator(self) {
		c.log.Debug("This node is not a validator")
		return
	}

	if c.validators.CustomValidators.GetCurrentProposer(c.Round) == self {
		c.log.Info("enterPropose: Our turn to propose.", " proposer ", self)
		c.doPropose(height, round)
	} else {
		c.log.Debug("enterPropose: Not our turn to propose.", " proposer ",
			c.validators.CustomValidators.GetCurrentProposer(c.Round), " self ", self)
	}
}

func (c *Core) doPropose(height int64, round int) {
	data := c.validators.CustomValidators.DecidesProposal()
	proposal := message.NewVote(message.ProposalType, height, round, &data, &c.lastCommittedData)

	if c.LockedRound > -1 && c.LockedProposal != nil {
		proposal.Proposed = c.LockedProposal.Proposed
	}

	c.signAddVote(proposal)
	c.Proposal = proposal
}

func (c *Core) fetchMissingVotes() {
	var fvr *message.FetchVotesReq
	var step RoundStepType
	if c.Step == RoundStepPrevoteFetch {
		fvr = c.Votes.Prevotes(c.Round).MakeFetchVotesReq()
		step = RoundStepPrevoteFetch
	} else if c.Step == RoundStepPrecommitFetch {
		fvr = c.Votes.Precommits(c.Round).MakeFetchVotesReq()
		step = RoundStepPrecommitFetch
	} else {
		return
	}
	c.validators.Sign(fvr)
	c.log.Debugf("fetchMissingVotes at height %d round %d", c.Height, c.Round)
	// randomly send the request to one neighbour
	c.validators.CustomValidators.Send(fvr, nil)

	c.scheduleTimeout(FetchInterval, c.Height, c.Round, step)
	c.inFetch = true
}

func (c *Core) enterPrevote(height int64, round int) {
	if c.Height != height || round < c.Round || (c.Round == round && RoundStepPrevote <= c.Step) {
		c.log.Debug(fmt.Sprintf("enterPrevote(%v/%v): Invalid args. Current step: %v/%v/%v", height, round, c.Height, c.Round, c.Step))
		return
	}

	c.log.Info(fmt.Sprintf("enterPrevote(%v/%v). Current: %v/%v/%v", height, round, c.Height, c.Round, c.Step))

	// Sign and broadcast vote as necessary
	c.doPrevote(height, round)
	c.updateRoundStep(round, RoundStepPrevote)

	c.enterPrevoteFetch(height, round)
	// Once `addVote` hits any +2/3 prevotes, we will go to PrevoteWait
}

// it calls fetchMissingVotes every sec unless any +2/3 prevotes received
func (c *Core) enterPrevoteFetch(height int64, round int) {
	c.log.Info(fmt.Sprintf("enterPrevoteFetch(%v/%v). Current: %v/%v/%v", height, round, c.Height, c.Round, c.Step))
	c.updateRoundStep(round, RoundStepPrevoteFetch)
	c.scheduleTimeout(FetchInterval, height, round, RoundStepPrevoteFetch)
}

func (c *Core) enterPrecommitFetch(height int64, round int) {
	c.log.Info(fmt.Sprintf("enterPrecommitFetch(%v/%v). Current: %v/%v/%v", height, round, c.Height, c.Round, c.Step))
	c.updateRoundStep(round, RoundStepPrecommitFetch)
	c.scheduleTimeout(FetchInterval, height, round, RoundStepPrecommitFetch)
}

// sign the vote, publish on internalMsgQueue and broadcast
func (c *Core) signAddVote(vote *message.Vote) {
	// if we're not a validator, do nothing
	if !c.isValidator() { // TODO: cache
		return
	}
	c.validators.Sign(vote)
	c.sendInternalMessage(msgInfo{vote, nil})
	c.validators.CustomValidators.BroadCast(vote)
}

func (c *Core) sendInternalMessage(mi msgInfo) {
	select {
	case c.msgQueue <- mi:
		c.log.Debugf("recv %v", mi.Msg)
	default:
		// NOTE: using the go-routine means our votes can
		// be processed out of order.
		// TODO: use CList here for strict determinism and
		// attempt push to internalMsgQueue in receiveRoutine
		c.log.Info("Internal msg queue is full. Using a go-routine")
		go func() { c.msgQueue <- mi }()
	}
}

func (c *Core) setByzantinePrevote(data *message.ProposedData) {
	c.byzantinePrevote = data
}

func (c *Core) doPrevote(height int64, round int) {
	var prevote *message.Vote

	if c.byzantinePrevote != nil && *c.byzantinePrevote != message.NilData {
		prevote = message.NewVote(message.PrevoteType, c.Height, c.Round, c.byzantinePrevote, &c.lastCommittedData)
		c.signAddVote(prevote)
		return
	}

	if c.LockedRound >= 0 && c.LockedProposal != nil {
		c.log.Info("enterPrevote: vote for POLed proposal: ", c.LockedProposal.Proposed)
		prevote = message.NewVote(message.PrevoteType, c.Height, c.Round, &c.LockedProposal.Proposed, &c.lastCommittedData)
	} else if c.Proposal != nil &&
		c.validators.CustomValidators.ValidateProposal(c.Proposal.Proposed) {
		prevote = message.NewVote(message.PrevoteType, c.Height, c.Round, &c.Proposal.Proposed, &c.lastCommittedData)
	} else {
		c.log.Info("enterPrevote: vote for nil")
		prevote = message.NewVote(message.PrevoteType, c.Height, c.Round, &message.NilData, &c.lastCommittedData)
	}

	c.signAddVote(prevote)
}

func (c *Core) enterPrevoteWait(height int64, round int) {
	if c.Height != height || round < c.Round || (c.Round == round && RoundStepPrevoteWait <= c.Step) {
		c.log.Debug(fmt.Sprintf("enterPrevoteWait(%v/%v): Invalid args. Current step: %v/%v/%v", height, round, c.Height, c.Round, c.Step))
		return
	}
	if !c.Votes.Prevotes(round).HasTwoThirdsAny() {
		common.PanicSanity(fmt.Sprintf("enterPrevoteWait(%v/%v), but Prevotes does not have any +2/3 votes", height, round))
	}
	c.log.Info(fmt.Sprintf("enterPrevoteWait(%v/%v). Current: %v/%v/%v", height, round, c.Height, c.Round, c.Step))

	defer func() {
		// Done enterPrevoteWait:
		c.updateRoundStep(round, RoundStepPrevoteWait)
	}()

	// Wait for some more prevotes; enterPrecommit
	c.scheduleTimeout(c.cfg.Prevote(round), height, round, RoundStepPrevoteWait)
}

func (c *Core) enterPrecommit(height int64, round int) {
	if c.Height != height || round < c.Round || (c.Round == round && RoundStepPrecommit <= c.Step) {
		c.log.Debug(fmt.Sprintf("enterPrecommit(%v/%v): Invalid args. Current step: %v/%v/%v", height, round, c.Height, c.Round, c.Step))
		return
	}

	c.log.Info(fmt.Sprintf("enterPrecommit(%v/%v). Current: %v/%v/%v", height, round, c.Height, c.Round, c.Step))

	defer func() {
		// Done enterPrecommit:
		c.updateRoundStep(round, RoundStepPrecommit)
		c.enterPrecommitFetch(c.Height, c.Round)
	}()

	// check for a polkaData
	polkaData, ok := c.Votes.Prevotes(round).TwoThirdsMajority()

	precommit := message.NewVote(message.PrecommitType, height, round, &message.NilData, &c.lastCommittedData)
	// If we don't have a polkaData, we must precommit nil.
	if !ok {
		if c.LockedProposal != nil {
			c.log.Info("enterPrecommit: No +2/3 prevotes during enterPrecommit while we're locked. Precommitting nil")
		} else {
			c.log.Info("enterPrecommit: No +2/3 prevotes during enterPrecommit. Precommitting nil.")
		}
		c.signAddVote(precommit)
		return
	}

	// the latest POLRound should be this round.
	polRound, _ := c.Votes.POLInfo()
	if polRound != round {
		common.PanicSanity(fmt.Sprintf("This POLRound should be %v but got %v", round, polRound))
	}

	// +2/3 prevoted nil. Unlock and precommit nil.
	if polkaData == message.NilData {
		if c.LockedProposal == nil {
			c.log.Info("enterPrecommit: +2/3 prevoted for nil.")
		} else {
			c.log.Info("enterPrecommit: +2/3 prevoted for nil. Unlocking")
			c.LockedRound = -1
			c.LockedProposal = nil
		}
		c.signAddVote(precommit)
		return
	}

	// At this point, +2/3 prevoted for a particular proposal.

	// If we're already locked on that proposed data, precommit it, and update the LockedRound
	if c.LockedRound >= 0 && c.LockedProposal.Proposed == polkaData {
		c.log.Info("enterPrecommit: +2/3 prevoted locked block. Relocking")
		c.LockedRound = round
		precommit.Proposed = polkaData
		c.signAddVote(precommit)
		return
	}

	// If +2/3 prevoted for proposal block, stage and precommit it
	if c.Proposal != nil && c.Proposal.Proposed == polkaData {
		c.log.Info("enterPrecommit: +2/3 prevoted proposal block. Locking", " proposed ", polkaData)
		c.LockedRound = round
		c.LockedProposal = c.Proposal
		precommit.Proposed = polkaData
		c.signAddVote(precommit)
		return
	}

	// If we get here， it means:
	// our LockedProposal doesn't match the polka(this should never happen cuz once we got polka,
	// lock on different proposed data is released)
	if c.LockedRound >= 0 && c.LockedProposal.Proposed != polkaData {
		c.log.Errorf("[enterPrecommit] we are locked on %v but receive polka on %v",
			c.LockedProposal.Proposed, polkaData)
	}

	c.log.Warnf("Got a polkaData %v but we don't have its proposal", polkaData)
	c.LockedRound = -1
	c.LockedProposal = nil

	c.signAddVote(precommit)
}

func (c *Core) enterPrecommitWait(height int64, round int) {
	if c.Height != height || round < c.Round || (c.Round == round && RoundStepPrecommitWait <= c.Step) {
		c.log.Debug(
			fmt.Sprintf(
				"enterPrecommitWait(%v/%v): Invalid args. "+
					"Current state is Height/Round/Step: %v/%v/%v",
				height, round, c.Height, c.Round, c.Step))
		return
	}
	if !c.Votes.Precommits(round).HasTwoThirdsAny() {
		common.PanicSanity(fmt.Sprintf("enterPrecommitWait(%v/%v), but Precommits does not have any +2/3 votes", height, round))
	}
	c.log.Info(fmt.Sprintf("enterPrecommitWait(%v/%v). Current: %v/%v/%v", height, round, c.Height, c.Round, c.Step))

	defer func() {
		// Done enterPrecommitWait:
		c.updateRoundStep(round, RoundStepPrecommitWait)
	}()

	// Wait for some more precommits; enterNewRound
	c.scheduleTimeout(c.cfg.Precommit(round), height, round, RoundStepPrecommitWait)
}

func (c *Core) enterCommit(height int64, commitRound int) {
	if c.Height != height || RoundStepCommit <= c.Step {
		c.log.Debug(fmt.Sprintf("enterCommit(%v/%v): Invalid args. Current step: %v/%v/%v", height, commitRound, c.Height, c.Round, c.Step))
		return
	}
	c.log.Info(fmt.Sprintf("enterCommit(%v/%v). Current: %v/%v/%v", height, commitRound, c.Height, c.Round, c.Step))

	maj23, ok := c.Votes.Precommits(commitRound).TwoThirdsMajority()
	if !ok {
		common.PanicSanity("RunActionCommit() expects +2/3 precommits")
	}

	c.CommitRound = commitRound
	c.CommitTime = common.Now()

	c.updateRoundStep(c.Round, RoundStepCommit)
	c.doCommit(maj23)
}

func (c *Core) doCommit(data message.ProposedData) {
	//self := c.validators.GetSelfPubKey()
	records := c.Votes.Precommits(c.CommitRound).MakeCommit()

	if data != records.ProposedData {
		common.PanicSanity("doCommit() inconsistent committed data")
	}

	// sign the Commit msg anyway as users might want to store it as an evidence
	records.CommitTime = c.CommitTime
	c.validators.Sign(records)

	if !c.hasRecvCommitRecords {
		c.validators.CustomValidators.BroadCast(records)
	}

	c.validators.CustomValidators.Commit(records)

	appState := c.validators.CustomValidators.GetAppState()
	c.updateToAppState(appState)
	c.hasRecvCommitRecords = false

	// c.StartTime is already set.
	// Schedule Round0 to start soon.
	c.scheduleRound0(&c.RoundState)
}

// Attempt to add the vote. if its a duplicate signature, dupeout the validator
func (c *Core) tryAddVote(vote *message.Vote) (bool, error) {
	added, err := c.addVote(vote)
	if err != nil {
		// If the vote height is off, we'll just ignore it,
		// But if it's a conflicting sig, add it to the c.evpool.
		// If it's otherwise invalid, punish peer.
		if err == ErrVoteHeightMismatch {
			return added, err
		} else if _, ok := err.(*ErrVoteConflictingVotes); ok {
			// TODO: catch conflict votes
			return added, err
		} else {
			// Probably an invalid signature / Bad peer.
			// Seems this can also err sometimes with "Unexpected step" - perhaps not from a bad peer ?
			c.log.Warn("Error attempting to add vote", " err ", err)
			return added, ErrAddingVote
		}
	}
	return added, nil
}

func (c *Core) addVote(vote *message.Vote) (added bool, err error) {
	c.log.Debug("addVote ", " voteHeight ", vote.Height, " voteType ", vote.Type, " cHeight ", c.Height)

	// A precommit for the previous height?
	// These come in while we wait timeoutCommit
	if vote.Height+1 == c.Height {
		if !(c.Step == RoundStepNewHeight && vote.Type == message.PrecommitType) ||
			c.LastCommit == nil {
			// fmt.Errorf("tryAddVote: Wrong height, not a LastCommit straggler commit.")
			return added, ErrVoteHeightMismatch
		}
		added, err = c.LastCommit.AddVote(vote)
		if !added {
			return added, err
		}

		c.log.Info(fmt.Sprintf("Added to lastPrecommits: %v", c.LastCommit.String()))

		// if we can skip timeoutCommit and have all the votes now,
		if c.cfg.SkipTimeoutCommit && c.LastCommit.HasAll() {
			// go straight to new round (skip timeout commit)
			// c.scheduleTimeout(time.Duration(0), c.Height, 0, ctypes.RoundStepNewHeight)
			c.enterNewRound(c.Height, 0)
		}

		return
	}

	/* height catch up is deprecated now
	if vote.Height > c.Height {
		// If this validator never committed before, it's almost certainly that
		// it just started and fell far behind the rest. Let it collect votes
		// from higher height so that it can catch up
		if c.LastCommit == nil || vote.Height < c.Height+128 {
			if vote.Type != message.ProposalType {
				c.stateSync.AddVote(vote)
			}
			return
		}
	}
	*/

	if vote.Height != c.Height {
		// Height mismatch is ignored.
		// Not necessarily a bad peer, but not favourable behaviour.
		err = ErrVoteHeightMismatch
		c.log.Info("Vote ignored and not added", " voteHeight ", vote.Height, " cHeight ", c.Height, " err ", err)
		return
	}

	if vote.Type == message.ProposalType {
		//c.log.Debug("defaultSetProposal")
		err = c.defaultSetProposal(vote)
		return
	}

	// TODO: add watermark for round
	height := c.Height
	added, err = c.Votes.AddVote(vote)
	if !added {
		if err != nil {
			c.log.Debug("vote not added with error: ", err)
		}
		return
	}

	switch vote.Type {
	case message.PrevoteType:
		prevotes := c.Votes.Prevotes(vote.Round)
		c.log.Debug("Added to prevote", " vote ", vote, " prevotes ", prevotes.String())

		if polkaData, ok := prevotes.TwoThirdsMajority(); ok {
			c.log.Info("POLKA!!! ", prevotes.String())

			// There was a polkaData!
			// If we're locked but this is a recent polkaData, unlock.
			// Unlock if `c.LockedRound < vote.Round <= c.Round`
			if (c.LockedProposal != nil) &&
				(c.LockedRound < vote.Round) &&
				//(vote.Round <= c.Round) &&
				c.LockedProposal.Proposed != polkaData {

				c.log.Info("Unlocking because of POL.", " lockedRound ", c.LockedRound, " POLRound ", vote.Round)
				c.LockedRound = -1
				c.LockedProposal = nil
			}

			// NOTE: our proposal may be nil or not what received a polkaData..
			if polkaData != message.NilData && (vote.Round == c.Round) {
				if c.Proposal != nil && c.Proposal.Proposed != polkaData {
					c.log.Warn(
						"Polka. Valid ProposedData we don't know about. Set Proposal=nil",
						"expect proposal:", c.Proposal.Proposed, " polkaData proposal ", polkaData)
					// We're getting the wrong proposal.
					c.Proposal = nil
					// TODO: we might receive this proposal again from other validators
				}
			}
		}

		// If +2/3 prevotes for *anything* for future round:
		if c.Round < vote.Round && prevotes.HasTwoThirdsAny() {
			// Round-skip if there is any 2/3+ of votes ahead of us
			c.enterNewRound(height, vote.Round)
		} else if c.Round == vote.Round && RoundStepPrevote <= c.Step { // current round
			_, ok := prevotes.TwoThirdsMajority()
			if ok {
				c.enterPrecommit(height, vote.Round)
			} else if prevotes.HasTwoThirdsAny() {
				c.enterPrevoteWait(height, vote.Round)
			}
		} else if RoundStepPrevote > c.Step {
			// If the proposal is received, enter prevote of c.Round.
			if c.Proposal != nil {
				c.enterPrevote(height, c.Round)
			} else {
				c.log.Debugf("receive prevote for ProposedData (%v), but we don't have proposal", vote.Proposed)
			}
		}

	case message.PrecommitType:
		precommits := c.Votes.Precommits(vote.Round)
		c.log.Debug("Added to precommit", " vote ", vote, " precommits ", precommits.String())

		if precommits.HasTwoThirdsMajority() {
			// Executed as TwoThirdsMajority could be from a higher round
			c.enterNewRound(height, vote.Round)
			c.enterPrecommit(height, vote.Round)
			c.enterCommit(height, vote.Round)
			if c.cfg.SkipTimeoutCommit && precommits.HasAll() {
				c.enterNewRound(c.Height, 0)
			}
		} else if c.Round <= vote.Round && precommits.HasTwoThirdsAny() {
			c.enterNewRound(height, vote.Round)
			c.enterPrecommitWait(height, vote.Round)
		}

	default:
		panic(fmt.Sprintf("Unexpected vote type %X", vote.Type)) // go-wire should prevent this.
	}

	return
}

func (c *Core) defaultSetProposal(proposal *message.Vote) error {
	// Already have one
	if c.Proposal != nil {
		//c.log.Debugf("Already got proposal %v from %s, get another proposal %v from %s",
		//	c.Proposal.Proposed, c.Proposal.Address, proposal.Proposed, proposal.Address)
		return nil
	}

	// Does not apply
	if proposal.Height != c.Height || proposal.Round != c.Round {
		c.log.Warn("proposal height or round mismatch ", proposal)
		return nil
	}

	if proposal.Prev != c.lastCommittedData {
		c.log.Warn("proposal with invalid base", proposal)
		return nil
	}

	// check if proposal is from the current proposer
	if c.validators.CustomValidators.GetCurrentProposer(c.Round) != proposal.Address {
		c.log.Errorf("invalid proposer. want %v, got %v",
			c.validators.CustomValidators.GetCurrentProposer(c.Round), proposal.Address)
		return ErrInvalidProposer
	}

	// Verify signature
	if !c.validators.VerifySignature(proposal) {
		c.log.Error("invalid sig ", proposal)
		return ErrInvalidProposalSignature
	}

	// Only accept the proposal and set Core.Proposal when CustomValidators approves it
	if c.validators.CustomValidators.ValidateProposal(proposal.Proposed) {
		c.Proposal = proposal
		c.log.Debug("Accept proposal", " proposal ", proposal)
		c.enterPrevote(c.Height, c.Round)
	} else {
		c.log.Warnf("invalid proposal, want %v got %v",
			c.validators.CustomValidators.DecidesProposal(), proposal.Proposed)
	}
	return nil
}

// Attempt to schedule a timeout (by sending timeoutInfo on the tickChan)
func (c *Core) scheduleTimeout(duration time.Duration, height int64, round int, step RoundStepType) {
	c.log.Debugf("++++++scheduleTimeout (%v/%d/%d/%v)", duration, height, round, step)
	c.timeoutTicker.ScheduleTimeout(timeoutInfo{duration, height, round, step})
}
